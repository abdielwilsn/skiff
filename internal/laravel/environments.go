package laravel

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"skiff/internal/executor"
	"skiff/internal/shell"
	"skiff/internal/store"
)

// SeedEnv writes a site's shared .env before its first deploy, so the deploy
// uses it instead of creating one from .env.example. An existing .env is
// kept.
func (d *Deployer) SeedEnv(ctx context.Context, w io.Writer, site store.Site, content string) error {
	script := fmt.Sprintf(`SH=%s/shared
mkdir -p "$SH"
if [ -e "$SH/.env" ]; then echo "Keeping the existing .env"; exit 0; fi
tmp=$(mktemp "$SH/.env.skiff.XXXXXX")
cat > "$tmp"
chmod 640 "$tmp"
mv "$tmp" "$SH/.env"
echo "Wrote $SH/.env"`, shell.Quote(SitePath(site.Domain)))
	return d.run(ctx, w, script, executor.Options{User: SiteUser, Stdin: strings.NewReader(content)})
}

var envLineRe = regexp.MustCompile(`^(\s*(?:export\s+)?)([A-Za-z_][A-Za-z0-9_]*)(\s*=\s*)(.*)$`)

// hostKeyRe matches variables that hold the site's own address (APP_URL,
// ALLOWED_HOSTS, SESSION_DOMAIN, NEXT_PUBLIC_SITE_URL…), whose values follow
// the environment's domain. Database, cache and mail settings never do.
var (
	hostKeyRe  = regexp.MustCompile(`(^|_)(URL|URLS|HOST|HOSTS|DOMAIN|DOMAINS|ORIGIN|ORIGINS)$`)
	keepKeysRe = regexp.MustCompile(`(^|_)(DB|DATABASE|REDIS|MAIL|SMTP|MEMCACHED|QUEUE|CACHE|BROKER|RABBITMQ|POSTGRES|MYSQL|MONGO|ELASTICSEARCH|S3|AWS|SENTRY)(_|$)`)
)

// EnvForEnvironment adapts a copy of production's .env for another
// environment: APP_ENV becomes the environment's name, APP_URL its address
// (scheme://toDomain), and the production
// domain becomes the environment's in variables that hold the site's own
// address. It returns the new content and the names of the variables it
// changed (never their values).
func EnvForEnvironment(content, env, fromDomain, toDomain, scheme string) (string, []string) {
	domainRe := regexp.MustCompile(`(^|[^A-Za-z0-9-])` + regexp.QuoteMeta(fromDomain) + `($|[^A-Za-z0-9-])`)
	var changed []string
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		m := envLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key, val := m[2], m[4]
		next := val
		switch {
		case key == "APP_ENV":
			next = env
		case key == "APP_URL":
			// The new domain has no certificate yet; turning HTTPS on updates it.
			next = scheme + "://" + toDomain
		case hostKeyRe.MatchString(key) && !keepKeysRe.MatchString(key):
			next = domainRe.ReplaceAllString(val, "${1}"+toDomain+"${2}")
		}
		if next != val {
			lines[i] = m[1] + key + m[3] + next
			changed = append(changed, key)
		}
	}
	return strings.Join(lines, "\n"), changed
}
