package laravel

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"skiff/internal/executor"
	"skiff/internal/github"
	"skiff/internal/shell"
	"skiff/internal/store"
)

// Release-based deploys. Each site directory looks like:
//
//	/home/skiff/<domain>/
//	  current -> releases/20261122141530   what Nginx serves
//	  releases/<id>/                        one checkout per deploy
//	  shared/.env, shared/storage/,         state every release links to
//	  shared/database/database.sqlite
//	  repo/                                 git cache, so fetches stay small
//
// A deploy builds a new release next to the live one and only switches
// "current" (an atomic rename) once the deploy script has succeeded, so a
// failed deploy never touches the live site. Rollback points "current" at an
// earlier release.

// KeepReleases is how many successful releases are kept for rollback.
const KeepReleases = 5

var releaseIDRe = regexp.MustCompile(`^[0-9]{14}$`)

// ValidReleaseID reports whether id looks like a release directory name.
func ValidReleaseID(id string) bool { return releaseIDRe.MatchString(id) }

// Release is one deployed release of a site.
type Release struct {
	ID       string    // directory name, a UTC timestamp
	Revision string    // "abc1234 commit subject"
	Current  bool      // what the site serves now
	Built    time.Time // parsed from ID
}

var releaseClock struct {
	sync.Mutex
	last string
}

// newReleaseID returns a UTC timestamp, unique even for back-to-back deploys.
func newReleaseID() string {
	releaseClock.Lock()
	defer releaseClock.Unlock()
	t := time.Now().UTC()
	id := t.Format("20060102150405")
	for id <= releaseClock.last {
		t = t.Add(time.Second)
		id = t.Format("20060102150405")
	}
	releaseClock.last = id
	return id
}

// locate defines shell variables for a site's paths:
// SITE, APP (the live code: current/, or the site dir itself for a site that
// hasn't had a release-based deploy yet) and ENVF (the .env file).
func locate(domain string) string {
	return fmt.Sprintf(`SITE=%s
if [ -e "$SITE/current" ]; then APP="$SITE/current"; else APP="$SITE"; fi
if [ -d "$SITE/shared" ]; then ENVF="$SITE/shared/.env"; else ENVF="$SITE/.env"; fi
`, shell.Quote(SitePath(domain)))
}

// Deploy fetches the site's branch into a new release, prepares it (shared
// .env/storage/SQLite, optional MySQL database), runs the deploy script in
// it, and only then makes it live. token may be empty for public repos; if db
// is non-nil the database is created and written into .env first.
func (d *Deployer) Deploy(ctx context.Context, w io.Writer, site store.Site, token string, db *DBSpec) error {
	q := shell.Quote
	dir := SitePath(site.Domain)
	id := newReleaseID()
	rel := dir + "/releases/" + id

	// 1. Code. Only this step sees the GitHub token; the deploy script (and the
	// composer packages it runs) never do.
	step(w, "Fetching %s (%s)", site.Repo, site.Branch)
	fetch := fmt.Sprintf(`SITE=%[1]s
REPO="$SITE/repo"
REL=%[4]s
mkdir -p "$SITE/releases"
if [ -d "$REPO/.git" ]; then
    git -C "$REPO" fetch --depth=1 origin %[2]s
    git -C "$REPO" reset -q --hard FETCH_HEAD
else
    git clone -q --depth=1 --single-branch --branch %[2]s %[3]s "$REPO"
fi
git -C "$REPO" log -1 --format='Commit %%h: %%s (%%an, %%cr)'
mkdir "$REL"
git --git-dir="$REPO/.git" --work-tree="$REL" checkout -f HEAD -- .
git -C "$REPO" log -1 --format='%%h %%s' > "$REL/.skiff-revision"
echo "Building release %[5]s"`, q(dir), q(site.Branch), q(github.CloneURL(site.Repo)), q(rel), id)
	if err := d.run(ctx, w, fetch, executor.Options{User: SiteUser, Env: github.GitEnv(token)}); err != nil {
		if token == "" {
			fmt.Fprintln(w, "\nHint: private repositories need GitHub connected in Settings → GitHub.")
		} else {
			fmt.Fprintln(w, "\nHint: if git couldn't read a username, GitHub rejected Skiff's access token (revoked, or the OAuth app changed). Disconnect and reconnect GitHub in Settings → GitHub.")
		}
		d.discardRelease(ctx, w, rel)
		return fmt.Errorf("fetch code: %w", err)
	}

	if err := d.buildRelease(ctx, w, site, dir, rel, id, db); err != nil {
		d.discardRelease(ctx, w, rel)
		fmt.Fprintln(w, "\nThe live site was not changed.")
		return err
	}

	// 2. Go live: an atomic rename of the "current" link.
	prev := d.liveRelease(ctx, dir)
	if err := d.switchTo(ctx, w, dir, id); err != nil {
		d.discardRelease(ctx, w, rel)
		return fmt.Errorf("switch release: %w", err)
	}

	// 3. Web server, then the site's processes.
	if err := d.configureNginx(ctx, w, site); err != nil {
		return fmt.Errorf("configure nginx: %w", err)
	}
	switch {
	case site.IsNode():
		if err := d.StartApp(ctx, w, site); err != nil {
			// The new release doesn't start: put the previous one back.
			if prev != "" {
				step(w, "The new release didn't start; switching back to %s", prev)
				if rerr := d.switchTo(ctx, w, dir, prev); rerr == nil {
					if serr := d.StartApp(ctx, w, site); serr == nil {
						d.discardRelease(ctx, w, rel)
						return fmt.Errorf("the app didn't start, so the previous release (%s) is live again: %w", prev, err)
					}
				}
			}
			return fmt.Errorf("start app: %w", err)
		}
	case site.IsLaravel():
		if site.WorkerCount > 0 || site.Scheduler {
			if err := d.ApplyLaravelProcesses(ctx, w, site); err != nil {
				return fmt.Errorf("the new release is live, but queue workers/scheduler failed: %w", err)
			}
		}
	}

	// 4. Housekeeping; failures here don't fail the deploy.
	step(w, "Cleaning up")
	if err := d.run(ctx, w, cleanupScript(dir, id, !site.IsNode()), executor.Options{User: SiteUser}); err != nil {
		fmt.Fprintf(w, "Cleanup skipped: %v\n", err)
	}
	fmt.Fprintf(w, "\nLive at %s://%s (release %s)\n", site.Scheme(), site.Domain, id)
	return nil
}

// liveRelease returns the id of the release "current" points at, or "".
func (d *Deployer) liveRelease(ctx context.Context, dir string) string {
	var out bytes.Buffer
	_ = d.Exec.Run(ctx, "readlink "+shell.Quote(dir+"/current")+" 2>/dev/null || true", executor.Options{User: SiteUser}, &out)
	id := strings.TrimPrefix(strings.TrimSpace(out.String()), "releases/")
	if !ValidReleaseID(id) {
		return ""
	}
	return id
}

// switchTo points "current" at a release with an atomic rename.
func (d *Deployer) switchTo(ctx context.Context, w io.Writer, dir, id string) error {
	step(w, "Switching to release %s", id)
	script := fmt.Sprintf(`cd %s
ln -sfn %s current.next
mv -Tf current.next current
echo "current -> $(readlink current)"`, shell.Quote(dir), shell.Quote("releases/"+id))
	return d.run(ctx, w, script, executor.Options{User: SiteUser})
}

// configureNginx writes the site's Nginx config if it doesn't exist yet, or
// brings the parts Skiff owns up to date (document root, PHP-FPM socket,
// header buffers) while keeping other edits, e.g. certbot's. Then it reloads
// Nginx, and PHP-FPM for Laravel sites (which also clears OPcache).
func (d *Deployer) configureNginx(ctx context.Context, w io.Writer, site store.Site) error {
	step(w, "Configuring Nginx")
	q := shell.Quote
	dir := SitePath(site.Domain)
	conf := q(NginxConfigPath(site.Domain))
	var update, reload string
	switch {
	case site.IsLaravel():
		update = fmt.Sprintf(`    if grep -q '^[[:space:]]*root %[2]s/public;' %[1]s; then
        sed -i 's#^\([[:space:]]*\)root %[2]s/public;#\1root %[2]s/current/public;#' %[1]s
        echo "Document root moved to %[2]s/current/public"
    fi
    sed -i -E 's#unix:/run/php/php[0-9.]+-fpm-%[3]s\.sock#unix:%[4]s#' %[1]s
    if ! grep -q 'fastcgi_buffer_size' %[1]s; then
        sed -i 's#^\([[:space:]]*\)fastcgi_hide_header X-Powered-By;#&\n\1fastcgi_buffer_size 64k;\n\1fastcgi_buffers 16 32k;#' %[1]s
        echo "Added larger FastCGI header buffers"
    fi`, conf, dir, SiteUser, fpmSocket(site.PHPVersion))
		reload = "\nservice php" + site.PHPVersion + "-fpm reload"
	case site.IsStatic():
		update = fmt.Sprintf(`    sed -i -E 's#^([[:space:]]*)root [^;]*;#\1root %[2]s;#' %[1]s`, conf, staticRoot(site))
	default:
		update = "    true"
	}
	script := fmt.Sprintf(`if [ ! -f %[1]s ]; then
    cat > %[1]s
    echo "Created %[1]s"
else
    echo "Keeping existing %[1]s"
%[3]s
fi
ln -sfn %[1]s %[2]s
nginx -t
service nginx reload%[4]s`, conf, q(nginxEnabledPath(site.Domain)), update, reload)
	return d.run(ctx, w, script, executor.Options{Stdin: strings.NewReader(NginxConfig(site))})
}

// buildRelease prepares the release directory and runs the deploy script in it.
func (d *Deployer) buildRelease(ctx context.Context, w io.Writer, site store.Site, dir, rel, id string, db *DBSpec) error {
	q := shell.Quote
	step(w, "Preparing environment")
	kind := site.Kind
	if kind == "" {
		kind = store.KindLaravel
	}
	prep := fmt.Sprintf(`SITE=%[1]s
REL=%[2]s
KIND=%[4]s
SH="$SITE/shared"
shopt -s dotglob nullglob
mkdir -p "$SH"

# A site deployed before release-based deploys keeps its code directly in
# $SITE. Move its state into shared/ and link it back, so the old code keeps
# working until this release goes live; nothing is copied or lost.
if [ "$KIND" = laravel ] && [ ! -e "$SITE/current" ] && [ -f "$SITE/artisan" ]; then
    echo "Moving this site to release-based deploys"
    if [ -f "$SITE/.env" ] && [ ! -L "$SITE/.env" ] && [ ! -e "$SH/.env" ]; then
        mv "$SITE/.env" "$SH/.env" && ln -s "$SH/.env" "$SITE/.env"
    fi
    if [ -d "$SITE/storage" ] && [ ! -L "$SITE/storage" ] && [ ! -e "$SH/storage" ]; then
        mv "$SITE/storage" "$SH/storage" && ln -s "$SH/storage" "$SITE/storage"
    fi
    if [ -f "$SITE/database/database.sqlite" ] && [ ! -L "$SITE/database/database.sqlite" ] && [ ! -e "$SH/database/database.sqlite" ]; then
        mkdir -p "$SH/database"
        mv "$SITE/database/database.sqlite" "$SH/database/database.sqlite" && ln -s "$SH/database/database.sqlite" "$SITE/database/database.sqlite"
    fi
fi

cd "$REL"

# .env: one shared copy, created on the first deploy.
if [ ! -f "$SH/.env" ]; then
    if [ -f .env.example ]; then
        echo "Creating .env from .env.example"
        cp .env.example "$SH/.env"
        if [ "$KIND" = laravel ]; then
            sed -i -e 's|^APP_ENV=.*|APP_ENV=production|' \
                   -e 's|^APP_DEBUG=.*|APP_DEBUG=false|' \
                   -e 's|^APP_URL=.*|APP_URL=http://%[3]s|' "$SH/.env"
        fi
    elif [ "$KIND" != laravel ]; then
        touch "$SH/.env"
    fi
fi
[ -f "$SH/.env" ] && chmod 640 "$SH/.env"
if [ -f "$SH/.env" ]; then rm -f .env && ln -s "$SH/.env" .env; fi

if [ "$KIND" != laravel ]; then echo "OK"; exit 0; fi
[ -f artisan ] || echo "Warning: no artisan file found; this does not look like a Laravel app."

# storage/: shared across releases (uploads, logs, sessions, cache).
if [ ! -d "$SH/storage" ]; then
    if [ -d storage ]; then cp -a storage "$SH/storage"; else mkdir -p "$SH/storage"; fi
fi
mkdir -p "$SH/storage/app/public" "$SH/storage/framework/cache/data" "$SH/storage/framework/sessions" \
         "$SH/storage/framework/views" "$SH/storage/logs"
rm -rf storage && ln -s "$SH/storage" storage

# Laravel's default SQLite database lives in shared/ too.
if [ -f "$SH/.env" ] && grep -q '^DB_CONNECTION=sqlite' "$SH/.env" && ! grep -q '^DB_DATABASE=' "$SH/.env"; then
    mkdir -p "$SH/database" database
    if [ ! -f "$SH/database/database.sqlite" ]; then
        echo "Creating SQLite database"
        touch "$SH/database/database.sqlite"
    fi
    ln -sfn "$SH/database/database.sqlite" database/database.sqlite
fi
echo "OK"`, q(dir), q(rel), site.Domain, kind)
	if err := d.run(ctx, w, prep, executor.Options{User: SiteUser}); err != nil {
		return fmt.Errorf("prepare environment: %w", err)
	}

	if db != nil {
		if err := d.CreateDatabase(ctx, w, *db); err != nil {
			return err
		}
		if err := d.WriteDatabaseEnv(ctx, w, site, *db); err != nil {
			return err
		}
	}

	if site.IsLaravel() {
		if err := d.EnsurePHP(ctx, w, site.PHPVersion); err != nil {
			return err
		}
	} else if site.NodeVersion == "" {
		return fmt.Errorf("choose a Node.js version for this site (Runtime card)")
	}
	if err := d.EnsureNode(ctx, w, rel, site.NodeVersion); err != nil {
		return err
	}

	step(w, "Running deploy script")
	env := []string{
		"SKIFF_SITE_PATH=" + rel, // the code being deployed (the script's working directory)
		"SKIFF_RELEASE=" + id,
		"SKIFF_SHARED_PATH=" + dir + "/shared",
		"SKIFF_BRANCH=" + site.Branch,
		"SKIFF_DOMAIN=" + site.Domain,
		"SKIFF_PHP=" + site.PHPVersion,
		"SKIFF_NODE=" + site.NodeVersion,
		"COMPOSER_NO_INTERACTION=1",
		"NPM_CONFIG_UPDATE_NOTIFIER=false",
		"NPM_CONFIG_FUND=false",
	}
	banner := ""
	if site.IsLaravel() {
		banner = `echo "Using PHP $(php -r 'echo PHP_VERSION;')"` + "\n"
	}
	if site.NodeVersion != "" {
		banner += `if command -v node >/dev/null 2>&1; then echo "Using Node.js $(node -v)"; fi` + "\n"
	}
	script := "set -eo pipefail\n" + withPHP(site.PHPVersion, withNode(site.NodeVersion, banner+site.DeployScript))
	if err := d.Exec.Run(ctx, script, executor.Options{User: SiteUser, Dir: rel, Env: env}, w); err != nil {
		return fmt.Errorf("deploy script: %w", err)
	}
	// Only releases whose deploy script succeeded can go live or be rolled back to.
	return d.run(ctx, w, "touch "+q(rel+"/.skiff-ok"), executor.Options{User: SiteUser})
}

// discardRelease removes a release that didn't make it (best effort).
func (d *Deployer) discardRelease(ctx context.Context, w io.Writer, rel string) {
	if !strings.Contains(rel, "/releases/") || !releaseIDRe.MatchString(rel[strings.LastIndex(rel, "/")+1:]) {
		return
	}
	script := fmt.Sprintf(`if [ -d %[1]s ]; then rm -rf -- %[1]s; echo "Discarded the unfinished release."; fi`, shell.Quote(rel))
	_ = d.run(context.WithoutCancel(ctx), w, script, executor.Options{User: SiteUser})
}

// cleanupScript keeps the newest KeepReleases successful releases (and the
// live one), drops node_modules from releases that aren't live, removes
// unfinished releases, and retires the pre-release checkout of a site that
// was just moved to release-based deploys.
func cleanupScript(dir, id string, stripNodeModules bool) string {
	return fmt.Sprintf(`SITE=%[1]s
STRIP=%[4]t
cd "$SITE"
shopt -s dotglob nullglob
LIVE="$(readlink current || true)"
kept=0
for r in $(ls -1 releases | sort -r); do
    path="releases/$r"
    [ "$path" = "$LIVE" ] && { kept=$((kept+1)); continue; }
    if [ ! -f "$path/.skiff-ok" ] || [ "$kept" -ge %[2]d ]; then
        rm -rf -- "$path"; echo "Removed release $r"; continue
    fi
    kept=$((kept+1))
    # Node apps need node_modules to run after a rollback; others only to build.
    if [ "$STRIP" = true ] && [ -d "$path/node_modules" ]; then rm -rf -- "$path/node_modules"; fi
done

# Old checkouts from before release-based deploys: retire the one from this
# move into .legacy-<id>, and delete ones left by earlier deploys.
for b in "$SITE"/.legacy-*; do rm -rf -- "$b"; echo "Removed old checkout $(basename "$b")"; done
if [ -f "$SITE/artisan" ]; then
    mkdir "$SITE/.legacy-%[3]s"
    for f in "$SITE"/*; do
        case "$(basename "$f")" in
            current|current.next|releases|shared|repo|.legacy-*) continue ;;
        esac
        mv -- "$f" "$SITE/.legacy-%[3]s/"
    done
    echo "Moved the pre-release checkout to .legacy-%[3]s (removed on the next deploy)"
fi
echo "Kept $kept release(s)"`, shell.Quote(dir), KeepReleases, id, stripNodeModules)
}

// Releases lists a site's finished releases, newest first.
func (d *Deployer) Releases(ctx context.Context, site store.Site) ([]Release, error) {
	script := locate(site.Domain) + `[ -d "$SITE/releases" ] || exit 0
LIVE="$(readlink "$SITE/current" 2>/dev/null || true)"
cd "$SITE/releases"
for r in *; do
    [ -f "$r/.skiff-ok" ] || continue
    rev="$(head -n 1 "$r/.skiff-revision" 2>/dev/null | cut -c1-200)"
    if [ "releases/$r" = "$LIVE" ]; then c=1; else c=0; fi
    printf '%s\t%s\t%s\n' "$r" "$c" "$rev"
done`
	var out bytes.Buffer
	if err := d.Exec.Run(ctx, script, executor.Options{User: SiteUser}, &out); err != nil {
		return nil, err
	}
	var list []Release
	for _, line := range strings.Split(out.String(), "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 || !ValidReleaseID(parts[0]) {
			continue // ignores anything that isn't a release line (e.g. dry-run output)
		}
		r := Release{ID: parts[0], Current: parts[1] == "1"}
		if len(parts) == 3 {
			r.Revision = parts[2]
		}
		r.Built, _ = time.Parse("20060102150405", r.ID)
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID > list[j].ID })
	return list, nil
}

// Rollback makes an earlier release live again. Database migrations made
// since are not undone. The release's cached config is rebuilt so it uses
// the current .env.
func (d *Deployer) Rollback(ctx context.Context, w io.Writer, site store.Site, id string) error {
	if !ValidReleaseID(id) {
		return fmt.Errorf("invalid release %q", id)
	}
	dir := SitePath(site.Domain)
	step(w, "Switching to release %s", id)
	sw := fmt.Sprintf(`cd %[1]s
[ -f %[2]s/.skiff-ok ] || { echo "Release %[3]s isn't available any more." >&2; exit 1; }
echo "Was: $(readlink current || echo none)"
ln -sfn %[2]s current.next
mv -Tf current.next current
echo "Now: $(readlink current)  ($(head -n 1 current/.skiff-revision 2>/dev/null))"
cd current
if [ -f bootstrap/cache/config.php ]; then php artisan config:cache; fi`, shell.Quote(dir), shell.Quote("releases/"+id), id)
	if err := d.run(ctx, w, withPHP(site.PHPVersion, sw), executor.Options{User: SiteUser}); err != nil {
		return err
	}
	switch {
	case site.IsNode():
		if err := d.StartApp(ctx, w, site); err != nil {
			return err
		}
	case site.IsLaravel():
		step(w, "Reloading PHP-FPM")
		if err := d.run(ctx, w, fmt.Sprintf("service php%s-fpm reload", site.PHPVersion), executor.Options{}); err != nil {
			return err
		}
		if site.WorkerCount > 0 || site.Scheduler {
			if err := d.ApplyLaravelProcesses(ctx, w, site); err != nil {
				return err
			}
		}
	}
	if site.IsLaravel() {
		fmt.Fprintln(w, "\nRolled back. Database migrations run since this release were not undone.")
	} else {
		fmt.Fprintln(w, "\nRolled back.")
	}
	return nil
}
