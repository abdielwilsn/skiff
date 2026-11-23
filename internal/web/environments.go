package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"skiff/internal/laravel"
	"skiff/internal/store"
)

// Environments. A production site can have other environments (staging,
// preview, …). Each is a site of its own, linked to production by ParentID:
// the same repository and project settings, but its own branch, domain,
// .env, database, releases and webhook. "Promote" deploys the exact commit an
// environment runs to production.

var envNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}$`)

func validEnvName(n string) bool { return envNameRe.MatchString(n) && n != store.EnvProduction }

// groupEnvironments splits sites into production sites and, per production
// site, its other environments.
func groupEnvironments(all []store.Site) ([]store.Site, map[int64][]store.Site) {
	var roots []store.Site
	envs := map[int64][]store.Site{}
	for _, x := range all {
		if x.IsProduction() {
			roots = append(roots, x)
		} else {
			envs[x.ParentID] = append(envs[x.ParentID], x)
		}
	}
	return roots, envs
}

type envRow struct {
	Site store.Site
	Live *laravel.Release // the release it serves; nil if unknown
}

type environmentsData struct {
	Site       store.Site // the site whose page this is
	Production store.Site
	Rows       []envRow
	Busy       bool

	// The new-environment form.
	Name, Domain, Branch string
	CopyEnv              bool
	Error                string
	Database             *store.Database // production's Skiff-managed database, if any
	MySQL                bool
}

// liveRelease returns the release a site serves, if it has been deployed.
func (s *Server) liveRelease(ctx context.Context, site store.Site) *laravel.Release {
	if site.DeployedAt == nil {
		return nil
	}
	rels, err := s.deployer.Releases(ctx, site)
	if err != nil {
		return nil
	}
	for i := range rels {
		if rels[i].Current {
			return &rels[i]
		}
	}
	return nil
}

// suggestedEnvDomain proposes <env>.<production domain>, or nothing for a
// production site served on an IP address.
func suggestedEnvDomain(env, domain string) string {
	if net.ParseIP(domain) != nil {
		return ""
	}
	return env + "." + strings.TrimPrefix(domain, "www.")
}

func (s *Server) environmentsData(ctx context.Context, site store.Site, d environmentsData) (environmentsData, error) {
	envs, err := s.st.Environments(site)
	if err != nil {
		return d, err
	}
	if len(envs) == 0 {
		return d, store.ErrNotFound
	}
	d.Site, d.Production = site, envs[0]
	d.Busy, _ = s.st.HasActiveJob(site.ID)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, e := range envs {
		d.Rows = append(d.Rows, envRow{Site: e, Live: s.liveRelease(ctx, e)})
	}
	if db, err := s.st.DatabaseForSite(d.Production.ID); err == nil {
		d.Database = &db
	}
	d.MySQL = s.mysqlInstalled()
	if d.Name == "" && d.Error == "" {
		d.Name, d.Branch, d.CopyEnv = "staging", "staging", true
		for _, e := range envs {
			if e.Environment == "staging" {
				d.Name, d.Branch = "preview", "preview"
			}
		}
		d.Domain = suggestedEnvDomain(d.Name, d.Production.Domain)
	}
	return d, nil
}

func (s *Server) environmentsPage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	d, err := s.environmentsData(r.Context(), site, environmentsData{})
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, "site_environments", http.StatusOK, sitePageFor(sess, site, "environments", "Environments", d))
}

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request, sess store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	prod := site
	if !site.IsProduction() {
		p, err := s.st.Site(site.ParentID)
		if err != nil {
			s.serverError(w, err)
			return
		}
		prod = p
	}
	d := environmentsData{
		Name:    strings.ToLower(strings.TrimSpace(r.FormValue("name"))),
		Domain:  strings.ToLower(strings.TrimSpace(r.FormValue("domain"))),
		Branch:  strings.TrimSpace(r.FormValue("branch")),
		CopyEnv: r.FormValue("copy_env") == "1",
	}
	envs, err := s.st.Environments(prod)
	if err != nil {
		s.serverError(w, err)
		return
	}
	nameTaken := false
	for _, e := range envs {
		if e.EnvName() == d.Name {
			nameTaken = true
		}
	}
	switch {
	case !validEnvName(d.Name):
		d.Error = "Name the environment with lowercase letters, digits and dashes, e.g. staging."
	case nameTaken:
		d.Error = prod.Domain + " already has a " + d.Name + " environment."
	case !validDomain(d.Domain):
		d.Error = "Enter a domain like staging.example.com."
	case d.Domain == s.panelHTTPSDomain():
		d.Error = d.Domain + " is where this panel is served. Use a different domain."
	case !validBranch(d.Branch):
		d.Error = "That branch name is not valid."
	}
	var env store.Site
	if d.Error == "" {
		env = store.Site{
			Domain: d.Domain, Repo: prod.Repo, Branch: d.Branch,
			PHPVersion: prod.PHPVersion, NodeVersion: prod.NodeVersion, PythonVersion: prod.PythonVersion,
			DeployScript: prod.DeployScript, Kind: prod.Kind,
			StartCommand: prod.StartCommand, OutputDir: prod.OutputDir,
			WorkerCount: prod.WorkerCount, WorkerOptions: prod.WorkerOptions, Scheduler: prod.Scheduler,
			Environment: d.Name, ParentID: prod.ID,
		}
		id, err := s.st.CreateSite(env)
		switch {
		case errors.Is(err, store.ErrDuplicate):
			d.Error = "A site for " + d.Domain + " already exists."
		case err != nil:
			s.serverError(w, err)
			return
		default:
			env.ID = id
		}
	}
	if d.Error != "" {
		d, err = s.environmentsData(r.Context(), site, d)
		if err != nil {
			s.serverError(w, err)
			return
		}
		s.render(w, r, "site_environments", http.StatusUnprocessableEntity, sitePageFor(sess, site, "environments", "Environments", d))
		return
	}

	// A Laravel site whose MySQL database Skiff manages gets a separate one,
	// so the environment's migrations and test data never touch production's.
	var ownDB string
	if _, err := s.st.DatabaseForSite(prod.ID); err == nil && prod.IsLaravel() && s.mysqlInstalled() {
		db, err := s.newDatabaseRecord(laravel.DBNameFor(env.Domain), &env.ID)
		if err != nil {
			s.serverError(w, err)
			return
		}
		ownDB = db.Name
	}

	if d.CopyEnv {
		envID, prodID := env.ID, prod.ID
		_, err := s.runner.Enqueue(&envID, "env", "Copy "+prod.Domain+"'s environment variables to "+env.Domain,
			func(ctx context.Context, log io.Writer) error {
				return s.copyEnvironment(ctx, log, prodID, envID, ownDB)
			}, nil)
		if err != nil {
			s.serverError(w, err)
			return
		}
	}
	jobID, err := s.startDeploy(env, "new "+env.Environment+" environment")
	if err != nil {
		s.serverError(w, err)
		return
	}
	setFlash(w, "ok", "Created the "+env.Environment+" environment. Deploying "+env.Branch+" to "+env.Domain+" now.")
	redirect(w, r, "/jobs/"+strconv.FormatInt(jobID, 10))
}

// copyEnvironment seeds a new environment's .env from production's, with
// APP_ENV and the site's own URLs changed to the environment's.
func (s *Server) copyEnvironment(ctx context.Context, log io.Writer, prodID, envID int64, ownDB string) error {
	prod, err := s.st.Site(prodID)
	if err != nil {
		return err
	}
	env, err := s.st.Site(envID)
	if err != nil {
		return err
	}
	fmt.Fprintf(log, "==> Reading %s's .env\n", prod.Domain)
	content, err := s.deployer.ReadEnv(ctx, prod)
	if err != nil {
		return fmt.Errorf("read production's .env: %w", err)
	}
	if strings.TrimSpace(content) == "" {
		fmt.Fprintln(log, "Production has no .env yet; the first deploy creates one from .env.example.")
		return nil
	}
	out, changed := laravel.EnvForEnvironment(content, env.Environment, prod.Domain, env.Domain, env.Scheme())
	if len(changed) > 0 {
		fmt.Fprintf(log, "Changed for %s: %s\n", env.Environment, strings.Join(changed, ", "))
	}
	if ownDB != "" {
		fmt.Fprintf(log, "Database settings will point at the new database %s.\n", ownDB)
	} else if mentionsDatabase(out) {
		fmt.Fprintln(log, "Note: the database settings were copied unchanged, so this environment uses the same database as production. Change them on its Env variables page if it shouldn't.")
	}
	fmt.Fprintf(log, "==> Writing %s's .env\n", env.Domain)
	return s.deployer.SeedEnv(ctx, log, env, out)
}

var dbSettingRe = regexp.MustCompile(`(?m)^\s*(DB_HOST|DB_DATABASE|DATABASE_URL|POSTGRES_[A-Z_]+|MYSQL_[A-Z_]+)\s*=\s*\S`)

// mentionsDatabase reports whether a .env sets a database server or name.
// SQLite paths relative to the app are per site, so they don't count.
func mentionsDatabase(env string) bool {
	for _, m := range dbSettingRe.FindAllStringSubmatch(env, -1) {
		if m[1] != "DB_DATABASE" {
			return true
		}
	}
	return regexp.MustCompile(`(?m)^\s*DB_CONNECTION\s*=\s*(mysql|mariadb|pgsql)`).MatchString(env)
}

// promote deploys the commit an environment serves to production.
func (s *Server) promote(w http.ResponseWriter, r *http.Request, _ store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	back := s.siteURL(site) + "/environments"
	if site.IsProduction() {
		setFlash(w, "err", "Production is already production. Promote from one of its other environments.")
		redirect(w, r, back)
		return
	}
	prod, err := s.st.Site(site.ParentID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	live := s.liveRelease(ctx, site)
	cancel()
	want := r.FormValue("commit")
	switch {
	case live == nil:
		setFlash(w, "err", site.Domain+" has nothing live to promote yet. Deploy it first.")
	case live.Commit == "":
		setFlash(w, "err", "This release was built before Skiff recorded full commits. Deploy "+site.Domain+" once more, then promote it.")
	case want != "" && want != live.Commit:
		setFlash(w, "err", site.Domain+" was deployed again since you loaded the page. Check what it runs now, then promote again.")
	}
	if live == nil || live.Commit == "" || (want != "" && want != live.Commit) {
		redirect(w, r, back)
		return
	}
	if busy, _ := s.st.HasActiveJob(prod.ID); busy {
		setFlash(w, "err", prod.Domain+" has a job queued or running. Wait for it to finish, then promote.")
		redirect(w, r, back)
		return
	}
	jobID, err := s.startDeployCommit(prod, "promote "+live.Commit[:7]+" from "+site.Environment, live.Commit)
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(jobID, 10))
}
