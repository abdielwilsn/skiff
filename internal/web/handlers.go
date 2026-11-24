package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"skiff/internal/github"
	"skiff/internal/laravel"
	"skiff/internal/store"
)

func (s *Server) serverError(w http.ResponseWriter, err error) {
	slog.Error("request failed", "err", err)
	http.Error(w, "Something went wrong: "+err.Error(), http.StatusInternalServerError)
}

func (s *Server) provisioned() bool {
	v, _ := s.st.Setting(keyProvisioned)
	return v == "1" || s.cfg.DryRun
}

// ---- dashboard -------------------------------------------------------------

type dashboardData struct {
	Provisioned  bool
	ProvisionJob *store.Job
	PHPVersion   string
	Sites        []store.Site           // production sites
	Envs         map[int64][]store.Site // a production site's other environments
	Jobs         []store.Job
	GitHubLogin  string
	Health       *laravel.Health // latest one-minute sample; nil until there is one
	Alerts       []healthAlert
	LastBackup   *store.Backup
	BackupsOff   bool
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request, sess store.Session) {
	d := dashboardData{Provisioned: s.provisioned(), PHPVersion: laravel.DefaultPHP}
	if j, err := s.st.LatestJob("provision"); err == nil {
		d.ProvisionJob = &j
	}
	all, err := s.st.Sites()
	if err != nil {
		s.serverError(w, err)
		return
	}
	d.Sites, d.Envs = groupEnvironments(all)
	if d.Jobs, err = s.st.RecentJobs(8); err != nil {
		s.serverError(w, err)
		return
	}
	d.GitHubLogin, _ = s.st.Setting(keyGitHubLogin)
	d.Health, _ = s.healthSnapshot()
	d.Alerts = healthAlerts(d.Health)
	if list, err := s.st.Backups(1); err == nil && len(list) > 0 {
		d.LastBackup = &list[0]
	}
	d.BackupsOff = !s.backupSettings().Enabled
	s.render(w, r, "dashboard", http.StatusOK, pageFor(sess, "Sites", "sites", d))
}

func (s *Server) provision(w http.ResponseWriter, r *http.Request, _ store.Session) {
	if j, err := s.st.LatestJob("provision"); err == nil && !j.Done() {
		redirect(w, r, "/jobs/"+strconv.FormatInt(j.ID, 10))
		return
	}
	id, err := s.runner.Enqueue(nil, "provision", "Provision server (Nginx, PHP "+laravel.DefaultPHP+", Composer)",
		s.deployer.Provision,
		func(err error) {
			if err == nil {
				_ = s.st.SetSetting(keyProvisioned, "1")
			}
		})
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(id, 10))
}

// ---- repositories ----------------------------------------------------------

func (s *Server) githubToken() string {
	t, _ := s.st.Setting(keyGitHubToken)
	return t
}

// repos lists the connected account's repositories, cached for two minutes.
func (s *Server) repos(ctx context.Context, refresh bool) ([]github.Repo, error) {
	token := s.githubToken()
	if token == "" {
		return nil, nil
	}
	s.repoMu.Lock()
	defer s.repoMu.Unlock()
	if !refresh && s.repoToken == token && time.Since(s.repoAt) < 2*time.Minute {
		return s.repoList, nil
	}
	list, err := github.ListRepos(ctx, token)
	if errors.Is(err, github.ErrUnauthorized) {
		_ = s.st.DeleteSettings(keyGitHubToken, keyGitHubLogin)
		return nil, errors.New("GitHub access was revoked. Connect GitHub again in Settings.")
	}
	if err != nil {
		return nil, err
	}
	s.repoToken, s.repoAt, s.repoList = token, time.Now(), list
	return list, nil
}

func (s *Server) forgetRepos() {
	s.repoMu.Lock()
	s.repoToken, s.repoList = "", nil
	s.repoMu.Unlock()
}

type reposData struct {
	Connected  bool
	Login      string
	Repos      []github.Repo
	Error      string
	SiteByRepo map[string]int64
}

func (s *Server) reposPage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	d := reposData{SiteByRepo: map[string]int64{}}
	d.Login, _ = s.st.Setting(keyGitHubLogin)
	d.Connected = s.githubToken() != ""
	if d.Connected {
		list, err := s.repos(r.Context(), r.URL.Query().Has("refresh"))
		if err != nil {
			d.Error = err.Error()
		}
		d.Repos = list
		d.Connected = s.githubToken() != ""
	}
	sites, _ := s.st.Sites()
	for _, x := range sites {
		if _, seen := d.SiteByRepo[x.Repo]; !seen || x.IsProduction() {
			d.SiteByRepo[x.Repo] = x.RootID()
		}
	}
	s.render(w, r, "repos", http.StatusOK, pageFor(sess, "Repositories", "repos", d))
}

// ---- sites -----------------------------------------------------------------

type newSiteData struct {
	Repo, Branch, Domain, Error string
	Repos                       []github.Repo
	Connected, Provisioned      bool
	Host                        string
	Engines                     []string // installed database engines
	Engine                      string   // database to create: "mysql", "postgres" or ""
	EngineSet                   bool     // the form was submitted, so Engine "" means none
	Redis                       bool     // Redis is installed
	UseRedis                    bool
	PHP                         string
	PHPVersions                 []string
	Node                        string
	NodeVersions                []string
	Python                      string
	PythonVersions              []string
	NodeSet                     bool // the form was submitted, so Node "" means "none"
	Framework                   string
	Frameworks                  []laravel.Framework
	StartCommand, OutputDir     string
}

func (s *Server) newSiteForm(r *http.Request, d newSiteData) newSiteData {
	d.Connected = s.githubToken() != ""
	d.Provisioned = s.provisioned()
	d.Engines = s.engines()
	if !d.EngineSet && len(d.Engines) > 0 {
		d.Engine = d.Engines[0]
	}
	d.Redis = s.redisPassword() != ""
	d.PHPVersions = laravel.PHPVersions
	if d.PHP == "" {
		d.PHP = laravel.DefaultPHP
	}
	d.NodeVersions = laravel.NodeVersions
	if !d.NodeSet {
		d.Node = laravel.DefaultNode
	}
	d.PythonVersions = laravel.PythonVersions
	if d.Python == "" {
		d.Python = laravel.DefaultPython
	}
	d.Frameworks = laravel.Frameworks
	if d.Framework == "" {
		d.Framework = "laravel"
	}
	if d.Connected {
		d.Repos, _ = s.repos(r.Context(), false)
	}
	host := r.Host
	if i := strings.LastIndexByte(host, ':'); i > 0 && !strings.HasSuffix(host, "]") {
		host = host[:i]
	}
	d.Host = host
	return d
}

func (s *Server) newSitePage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	q := r.URL.Query()
	d := s.newSiteForm(r, newSiteData{Repo: q.Get("repo"), Branch: q.Get("branch")})
	s.render(w, r, "site_new", http.StatusOK, pageFor(sess, "New site", "sites", d))
}

func (s *Server) createSite(w http.ResponseWriter, r *http.Request, sess store.Session) {
	d := newSiteData{
		Repo:      normalizeRepo(r.FormValue("repo")),
		Branch:    strings.TrimSpace(r.FormValue("branch")),
		Domain:    strings.ToLower(strings.TrimSpace(r.FormValue("domain"))),
		Engine:    r.FormValue("database"),
		EngineSet: true,
		UseRedis:  r.FormValue("redis") == "1",
		PHP:       r.FormValue("php"),
		Node:      r.FormValue("node"),
		NodeSet:   true,
		Python:    r.FormValue("python"),

		Framework:    r.FormValue("framework"),
		StartCommand: strings.TrimSpace(r.FormValue("start_command")),
		OutputDir:    strings.Trim(strings.TrimSpace(r.FormValue("output_dir")), "/"),
	}
	if d.Branch == "" {
		d.Branch = "main"
	}
	if r.FormValue("mysql") == "1" && d.Engine == "" { // the form before PostgreSQL
		d.Engine = store.EngineMySQL
	}
	fw := laravel.FrameworkByID(d.Framework)
	d.Framework = fw.ID
	if fw.Kind != store.KindLaravel && !laravel.ValidPHP(d.PHP) {
		d.PHP = laravel.DefaultPHP // unused by other project types
	}
	if fw.Kind == store.KindPython {
		d.Node = "" // Python apps don't build with Node
	} else {
		d.Python = ""
	}
	if d.StartCommand == "" {
		d.StartCommand = fw.Start
	}
	if d.OutputDir == "" {
		d.OutputDir = fw.Output
	}
	switch {
	case !validRepo(d.Repo):
		d.Error = "Choose a repository, or type it as owner/name."
	case !validBranch(d.Branch):
		d.Error = "That branch name is not valid."
	case !validDomain(d.Domain):
		d.Error = "Enter a domain like app.example.com, or this server's IP address."
	case !laravel.ValidPHP(d.PHP):
		d.Error = "Choose a PHP version."
	case !laravel.ValidNode(d.Node):
		d.Error = "Choose a Node.js version."
	case d.Domain == s.panelHTTPSDomain():
		d.Error = d.Domain + " is where this panel is served. Use a different domain for the site."
	case (fw.Kind == store.KindNode || fw.Kind == store.KindStatic) && d.Node == "":
		d.Error = fw.Name + " needs Node.js; pick a version."
	case fw.Kind == store.KindPython && !laravel.ValidPython(d.Python):
		d.Error = "Choose a Python version."
	case (fw.Kind == store.KindNode || fw.Kind == store.KindPython) && !laravel.ValidStartCommand(d.StartCommand):
		d.Error = "Enter the command that starts your app, e.g. npm run start."
	case fw.Kind == store.KindStatic && !laravel.ValidOutputDir(d.OutputDir):
		d.Error = "Enter the folder your build writes to, e.g. dist (or . for the repository root)."
	case d.Engine != "" && fw.Kind != store.KindStatic && !s.engineInstalled(d.Engine):
		d.Error = "Install " + engineLabel(d.Engine) + " on the Databases page first, or choose another database."
	case d.UseRedis && fw.Kind != store.KindStatic && s.redisPassword() == "":
		d.Error = "Install Redis on the Databases page first."
	}
	if fw.Kind == store.KindStatic {
		d.Engine, d.UseRedis = "", false
	}
	if d.Error == "" {
		site := store.Site{
			Domain: d.Domain, Repo: d.Repo, Branch: d.Branch,
			PHPVersion: d.PHP, NodeVersion: d.Node, DeployScript: fw.DeployScript(),
			Kind: fw.Kind, PythonVersion: d.Python, Redis: d.UseRedis,
		}
		switch fw.Kind {
		case store.KindNode, store.KindPython:
			site.StartCommand = d.StartCommand
		case store.KindStatic:
			site.OutputDir = d.OutputDir
		}
		id, err := s.st.CreateSite(site)
		switch {
		case errors.Is(err, store.ErrDuplicate):
			d.Error = "A site for " + d.Domain + " already exists."
		case err != nil:
			s.serverError(w, err)
			return
		default:
			site.ID = id
			if d.Engine != "" {
				if _, err := s.newDatabaseRecord(d.Engine, laravel.DBNameFor(site.Domain), &id); err != nil {
					s.serverError(w, err)
					return
				}
			}
			if d.UseRedis {
				_ = s.st.SetSiteRedis(id, true)
				site.Redis = true
			}
			jobID, err := s.startDeploy(site, "")
			if err != nil {
				s.serverError(w, err)
				return
			}
			setFlash(w, "ok", "Site created. Deploying "+site.Repo+" now.")
			redirect(w, r, "/jobs/"+strconv.FormatInt(jobID, 10))
			return
		}
	}
	d = s.newSiteForm(r, d)
	s.render(w, r, "site_new", http.StatusUnprocessableEntity, pageFor(sess, "New site", "sites", d))
}

// startDeploy queues a deploy. reason (optional) is shown in the job title,
// e.g. "push a1b2c3d by abdiel".
func (s *Server) startDeploy(site store.Site, reason string) (int64, error) {
	return s.startDeployCommit(site, reason, "")
}

// startDeployCommit queues a deploy of one commit (a full hash), or of the
// branch tip if commit is "".
func (s *Server) startDeployCommit(site store.Site, reason, commit string) (int64, error) {
	if err := s.st.SetSiteStatus(site.ID, store.SiteDeploying); err != nil {
		return 0, err
	}
	siteID := site.ID
	var pendingDB int64 // database created by this deploy, if any
	title := "Deploy " + site.Domain
	if reason != "" {
		title += " (" + reason + ")"
	}
	return s.runner.Enqueue(&siteID, "deploy", title,
		func(ctx context.Context, log io.Writer) error {
			// Reload at run time: the deploy script may have changed while queued.
			cur, err := s.st.Site(siteID)
			if err != nil {
				return err
			}
			// A database chosen when the site was created is set up on its
			// first successful deploy, before migrations run.
			var spec *laravel.DBSpec
			if db, err := s.st.DatabaseForSite(siteID); err == nil && db.Status == store.DBPending {
				sp := laravel.SpecOf(db)
				spec, pendingDB = &sp, db.ID
			}
			// Back up the site's databases before its migrations run.
			if err := s.backupBeforeDeploy(ctx, log, cur); err != nil {
				return err
			}
			opt := laravel.DeployOptions{DB: spec, Commit: commit}
			// A site created with Redis is connected on its first deploy.
			if cur.Redis && cur.DeployedAt == nil {
				opt.RedisPassword = s.redisPassword()
			}
			return s.deployer.DeployWith(ctx, log, cur, s.githubToken(), opt)
		},
		func(err error) {
			if err != nil {
				_ = s.st.SetSiteStatus(siteID, store.SiteFailed)
				return
			}
			_ = s.st.MarkSiteDeployed(siteID)
			if pendingDB != 0 {
				_ = s.st.SetDatabaseStatus(pendingDB, store.DBReady)
			}
		})
}

// newDatabaseRecord reserves a unique database name based on base and records
// it (status pending, optionally attached to a site).
func (s *Server) newDatabaseRecord(engine, base string, siteID *int64) (store.Database, error) {
	name := base
	for i := 2; ; i++ {
		taken, err := s.st.NameTaken(name)
		if err != nil {
			return store.Database{}, err
		}
		if !taken {
			break
		}
		name = fmt.Sprintf("%s_%d", base, i)
	}
	db := store.Database{Engine: engine, Name: name, Username: name, Password: laravel.NewPassword(), Status: store.DBPending, SiteID: siteID}
	id, err := s.st.CreateDatabase(db)
	db.ID = id
	return db, err
}

// siteFromPath loads the site named by the {id} path value, writing a 404 if missing.
func (s *Server) siteFromPath(w http.ResponseWriter, r *http.Request) (store.Site, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.Site{}, false
	}
	site, err := s.st.Site(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return site, false
	}
	if err != nil {
		s.serverError(w, err)
		return site, false
	}
	return site, true
}

type siteData struct {
	Site      store.Site
	Jobs      []store.Job
	Busy      bool
	Path      string
	NginxPath string

	Database       *store.Database
	Releases       []laravel.Release
	ReleasesErr    string
	Process        laravel.ProcessStatus
	MaxWorkers     int
	PHPVersions    []string
	NodeVersions   []string
	PythonVersions []string
	RedisReady     bool   // Redis is installed on the server
	RedisPrefix    string // the key prefix a Laravel site gets

	HookBase        string // public HTTPS origin for webhooks; "" = not available
	GitHubConnected bool

	HTTPSEligible bool
	HTTPSReason   string // why HTTPS isn't possible, when not eligible
	CertExpires   *time.Time
	CertDaysLeft  int
	ACMEEmail     string
	ACMEStaging   bool // a non-production ACME server is configured
}

func (s *Server) sitePage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	d := s.siteBase(site)
	d.Jobs, _ = s.st.SiteJobs(site.ID, 6)
	if db, err := s.st.DatabaseForSite(site.ID); err == nil {
		d.Database = &db
	}
	d.PHPVersions = laravel.PHPVersions
	d.NodeVersions = laravel.NodeVersions
	d.PythonVersions = laravel.PythonVersions
	d.MaxWorkers = laravel.MaxWorkers
	d.RedisReady = s.redisPassword() != ""
	d.RedisPrefix = laravel.RedisPrefix(site.Domain)
	if site.DeployedAt != nil && !s.cfg.DryRun && (site.HasApp() || site.WorkerCount > 0 || site.Scheduler) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		d.Process, _ = s.deployer.Status(ctx, site)
		cancel()
	}
	s.render(w, r, "site", http.StatusOK, sitePageFor(sess, site, "overview", "", d))
}

func (s *Server) siteDeploymentsPage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	d := s.siteBase(site)
	d.Jobs, _ = s.st.SiteJobs(site.ID, 30)
	if site.DeployedAt != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		rels, err := s.deployer.Releases(ctx, site)
		cancel()
		if err != nil {
			d.ReleasesErr = "Couldn't list releases: " + err.Error()
		}
		d.Releases = rels
	}
	s.render(w, r, "site_deployments", http.StatusOK, sitePageFor(sess, site, "deployments", "Deployments", d))
}

func (s *Server) siteSettingsPage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	d := s.siteBase(site)
	d.HookBase = s.publicHTTPSBase()
	d.GitHubConnected = s.githubToken() != ""
	d.HTTPSEligible, d.HTTPSReason = laravel.HTTPSEligible(site.Domain)
	d.ACMEEmail, _ = s.st.Setting(keyACMEEmail)
	d.ACMEStaging = s.cfg.ACMEServer != ""
	if site.HTTPS && !s.cfg.DryRun {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		if t, err := s.deployer.CertExpiry(ctx, site); err == nil {
			d.CertExpires = &t
			d.CertDaysLeft = int(time.Until(t).Hours() / 24)
		}
		cancel()
	}
	s.render(w, r, "site_settings", http.StatusOK, sitePageFor(sess, site, "settings", "Settings", d))
}

// siteBase fills the fields every site page uses.
func (s *Server) siteBase(site store.Site) siteData {
	d := siteData{Site: site, Path: laravel.SitePath(site.Domain) + "/current", NginxPath: laravel.NginxConfigPath(site.Domain)}
	d.Busy, _ = s.st.HasActiveJob(site.ID)
	return d
}

func (s *Server) enableHTTPS(w http.ResponseWriter, r *http.Request, _ store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	fail := func(msg string) {
		setFlash(w, "err", msg)
		redirect(w, r, s.siteURL(site)+"/settings#https")
	}
	if ok, why := laravel.HTTPSEligible(site.Domain); !ok {
		fail(why)
		return
	}
	if site.DeployedAt == nil {
		fail("Deploy the site once before enabling HTTPS.")
		return
	}
	if busy, _ := s.st.HasActiveJob(site.ID); busy {
		fail("Wait for the running job to finish first.")
		return
	}
	email := strings.TrimSpace(r.FormValue("email"))
	if email != "" && !emailRe.MatchString(email) {
		fail("That email address doesn't look right.")
		return
	}
	if err := s.st.SetSetting(keyACMEEmail, email); err != nil {
		s.serverError(w, err)
		return
	}
	siteID := site.ID
	id, err := s.runner.Enqueue(&siteID, "https", "Enable HTTPS for "+site.Domain,
		func(ctx context.Context, log io.Writer) error {
			cur, err := s.st.Site(siteID)
			if err != nil {
				return err
			}
			return s.deployer.EnableHTTPS(ctx, log, cur, email)
		},
		func(err error) {
			if err == nil {
				_ = s.st.SetSiteHTTPS(siteID, true)
			}
		})
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(id, 10))
}

func (s *Server) disableHTTPS(w http.ResponseWriter, r *http.Request, _ store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	if busy, _ := s.st.HasActiveJob(site.ID); busy {
		setFlash(w, "err", "Wait for the running job to finish first.")
		redirect(w, r, s.siteURL(site)+"/settings#https")
		return
	}
	siteID := site.ID
	id, err := s.runner.Enqueue(&siteID, "https-off", "Remove HTTPS from "+site.Domain,
		func(ctx context.Context, log io.Writer) error {
			cur, err := s.st.Site(siteID)
			if err != nil {
				return err
			}
			cur.HTTPS = false // the restored Nginx config is plain HTTP
			return s.deployer.DisableHTTPS(ctx, log, cur)
		},
		func(err error) {
			if err == nil {
				_ = s.st.SetSiteHTTPS(siteID, false)
			}
		})
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(id, 10))
}

func (s *Server) siteURL(site store.Site) string { return "/sites/" + strconv.FormatInt(site.ID, 10) }

func (s *Server) deploySite(w http.ResponseWriter, r *http.Request, _ store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	if busy, _ := s.st.HasActiveJob(site.ID); busy {
		setFlash(w, "err", "A job for this site is already queued or running.")
		redirect(w, r, s.siteURL(site))
		return
	}
	id, err := s.startDeploy(site, "")
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(id, 10))
}

// setPHP changes the site's PHP version; it takes effect on the next deploy,
// which installs the version if needed and repoints Nginx at it.
func (s *Server) setPHP(w http.ResponseWriter, r *http.Request, _ store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	php := r.FormValue("php")
	if !laravel.ValidPHP(php) {
		setFlash(w, "err", "Choose a PHP version.")
		redirect(w, r, s.siteURL(site))
		return
	}
	if err := s.st.SetSitePHP(site.ID, php); err != nil {
		s.serverError(w, err)
		return
	}
	if php != site.PHPVersion {
		setFlash(w, "ok", "PHP "+php+" selected. Click Deploy now to switch the site over.")
	}
	redirect(w, r, s.siteURL(site))
}

// setNode changes the Node.js version used to build the site's assets ("" = none).
func (s *Server) setNode(w http.ResponseWriter, r *http.Request, _ store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	node := r.FormValue("node")
	if !laravel.ValidNode(node) {
		setFlash(w, "err", "Choose a Node.js version.")
		redirect(w, r, s.siteURL(site))
		return
	}
	if err := s.st.SetSiteNode(site.ID, node); err != nil {
		s.serverError(w, err)
		return
	}
	if node != site.NodeVersion {
		if node == "" {
			setFlash(w, "ok", "Node.js turned off for this site. Remove npm commands from the deploy script if it has any.")
		} else {
			setFlash(w, "ok", "Node.js "+node+" selected. It's installed on the next deploy.")
		}
	}
	redirect(w, r, s.siteURL(site))
}

// setPython changes a Python app's interpreter; it takes effect on the next deploy.
func (s *Server) setPython(w http.ResponseWriter, r *http.Request, _ store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	v := r.FormValue("python")
	if !site.IsPython() || !laravel.ValidPython(v) {
		setFlash(w, "err", "Choose a Python version.")
		redirect(w, r, s.siteURL(site))
		return
	}
	if err := s.st.SetSitePython(site.ID, v); err != nil {
		s.serverError(w, err)
		return
	}
	if v != site.PythonVersion {
		setFlash(w, "ok", "Python "+v+" selected. Click Deploy to build a release with it.")
	}
	redirect(w, r, s.siteURL(site))
}

// rollback makes an earlier release of the site live again.
func (s *Server) rollback(w http.ResponseWriter, r *http.Request, _ store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	id := r.FormValue("release")
	if !laravel.ValidReleaseID(id) {
		setFlash(w, "err", "Pick a release to roll back to.")
		redirect(w, r, s.siteURL(site)+"/deployments")
		return
	}
	if busy, _ := s.st.HasActiveJob(site.ID); busy {
		setFlash(w, "err", "Wait for the running job to finish first.")
		redirect(w, r, s.siteURL(site)+"/deployments")
		return
	}
	siteID := site.ID
	jobID, err := s.runner.Enqueue(&siteID, "rollback", "Roll back "+site.Domain+" to "+id,
		func(ctx context.Context, log io.Writer) error {
			cur, err := s.st.Site(siteID)
			if err != nil {
				return err
			}
			return s.deployer.Rollback(ctx, log, cur, id)
		},
		func(err error) {
			if err == nil {
				_ = s.st.SetSiteStatus(siteID, store.SiteActive)
			}
		})
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(jobID, 10))
}

func (s *Server) saveScript(w http.ResponseWriter, r *http.Request, _ store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	script := normalizeText(r.FormValue("script"))
	if len(script) > 64<<10 {
		setFlash(w, "err", "The deploy script is too long (64 KB max).")
		redirect(w, r, s.siteURL(site))
		return
	}
	if strings.TrimSpace(r.FormValue("reset")) == "1" {
		script = laravel.DefaultDeployScript
	}
	if err := s.st.SetDeployScript(site.ID, script); err != nil {
		s.serverError(w, err)
		return
	}
	setFlash(w, "ok", "Deploy script saved. It runs on the next deploy.")
	redirect(w, r, s.siteURL(site)+"/settings#script")
}

type envData struct {
	Site  store.Site
	Env   string
	Error string
}

func (s *Server) envPage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	d := envData{Site: site}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	env, err := s.deployer.ReadEnv(ctx, site)
	if err != nil {
		d.Error = "Could not read .env: " + err.Error()
	}
	d.Env = env
	s.render(w, r, "site_env", http.StatusOK, sitePageFor(sess, site, "env", "Env variables", d))
}

func (s *Server) saveEnv(w http.ResponseWriter, r *http.Request, _ store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.deployer.WriteEnv(ctx, site, normalizeText(r.FormValue("env"))); err != nil {
		setFlash(w, "err", "Could not save .env: "+err.Error())
		redirect(w, r, s.siteURL(site)+"/env")
		return
	}
	// Long-running processes read the environment when they start.
	if site.DeployedAt != nil && (site.HasApp() || site.WorkerCount > 0) {
		setFlash(w, "ok", ".env saved. Restarting so it takes effect.")
		s.enqueueSiteJob(w, r, site, "restart", "Restart "+site.Domain+" after .env change",
			func(ctx context.Context, log io.Writer, cur store.Site) error {
				if cur.HasApp() {
					return s.deployer.StartApp(ctx, log, cur)
				}
				return s.deployer.ApplyLaravelProcesses(ctx, log, cur)
			})
		return
	}
	setFlash(w, "ok", ".env saved.")
	redirect(w, r, s.siteURL(site)+"/env")
}

func (s *Server) deleteSite(w http.ResponseWriter, r *http.Request, _ store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	if busy, _ := s.st.HasActiveJob(site.ID); busy {
		setFlash(w, "err", "Wait for the running job to finish before deleting this site.")
		redirect(w, r, s.siteURL(site))
		return
	}
	if n, _ := s.st.ChildCount(site.ID); n > 0 {
		setFlash(w, "err", "Delete this site's other environments first (Environments page).")
		redirect(w, r, s.siteURL(site)+"/settings#danger")
		return
	}
	if r.FormValue("confirm") != site.Domain {
		setFlash(w, "err", "Type the domain exactly to confirm deletion.")
		redirect(w, r, s.siteURL(site)+"/settings#danger")
		return
	}
	if err := s.st.SetSiteStatus(site.ID, store.SiteDeleting); err != nil {
		s.serverError(w, err)
		return
	}
	siteID := site.ID
	id, err := s.runner.Enqueue(&siteID, "delete", "Delete "+site.Domain,
		func(ctx context.Context, log io.Writer) error {
			if site.HookID != 0 {
				fmt.Fprintln(log, "\n==> Removing the GitHub webhook")
				if err := s.removeWebhook(ctx, site); err != nil {
					fmt.Fprintf(log, "Couldn't remove it (%v); delete it in the repo's Settings → Webhooks.\n", err)
				} else {
					fmt.Fprintln(log, "Removed")
				}
			}
			return s.deployer.Delete(ctx, log, site)
		},
		func(err error) {
			if err != nil {
				_ = s.st.SetSiteStatus(siteID, store.SiteFailed)
			} else {
				_ = s.st.DeleteSite(siteID)
			}
		})
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(id, 10))
}

// ---- jobs ------------------------------------------------------------------

type jobData struct {
	Job  store.Job
	Site *store.Site
}

func (s *Server) jobFromPath(w http.ResponseWriter, r *http.Request) (store.Job, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.Job{}, false
	}
	j, err := s.st.Job(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return j, false
	}
	if err != nil {
		s.serverError(w, err)
		return j, false
	}
	return j, true
}

func (s *Server) jobPage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	j, ok := s.jobFromPath(w, r)
	if !ok {
		return
	}
	d := jobData{Job: j}
	if j.SiteID != nil {
		if site, err := s.st.Site(*j.SiteID); err == nil {
			d.Site = &site
		}
	}
	p := pageFor(sess, j.Title, "sites", d)
	if d.Site != nil && j.Kind != "delete" {
		p = sitePageFor(sess, *d.Site, "deployments", "Job #"+strconv.FormatInt(j.ID, 10), d)
	}
	s.render(w, r, "job", http.StatusOK, p)
}

func (s *Server) jobLog(w http.ResponseWriter, r *http.Request, _ store.Session) {
	j, ok := s.jobFromPath(w, r)
	if !ok {
		return
	}
	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if offset < 0 {
		offset = 0
	}
	data, size, err := s.runner.ReadLog(j.ID, offset)
	if err != nil {
		s.serverError(w, err)
		return
	}
	// Don't split a multi-byte character; the rest is sent on the next poll.
	if n := len(data); n > 0 {
		start := n - 1
		for start > 0 && start > n-4 && !utf8.RuneStart(data[start]) {
			start--
		}
		if !utf8.FullRune(data[start:]) {
			data = data[:start]
		}
	}
	next := offset + int64(len(data))
	link := ""
	if j.Kind == "panel-https" && j.Status == store.JobSuccess {
		s.panelMu.Lock()
		link = s.panelLink
		s.panelMu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data":     string(data),
		"offset":   next,
		"status":   j.Status,
		"done":     j.Done(),
		"more":     next < size,
		"duration": j.Duration(),
		"link":     link,
	})
}

// ---- settings & GitHub -----------------------------------------------------

type settingsData struct {
	ClientID    string
	SecretSet   bool
	Login       string
	Connected   bool
	CallbackURL string
	HomeURL     string

	PanelDomain  string // HTTPS domain the panel is served at, if any
	PanelBusy    bool   // a panel HTTPS job is queued or running
	PanelJobID   int64
	PanelPort    string
	FlagURL      string // --url overrides panel-domain handling
	Provisioned  bool
	ACMEEmail    string
	ViaHTTPS     bool
	CurrentHost  string
	PanelFailure *store.Job
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	d := settingsData{CallbackURL: s.baseURL(r) + "/github/callback", HomeURL: s.baseURL(r)}
	d.PanelDomain = s.panelHTTPSDomain()
	d.PanelPort = s.panelPort()
	d.FlagURL = s.cfg.BaseURL
	d.Provisioned = s.provisioned()
	d.ACMEEmail, _ = s.st.Setting(keyACMEEmail)
	d.ViaHTTPS = r.TLS != nil || (isLoopback(r) && r.Header.Get("X-Forwarded-Proto") == "https")
	d.CurrentHost = r.Host
	if j, err := s.st.LatestJob("panel-https"); err == nil {
		if !j.Done() {
			d.PanelBusy, d.PanelJobID = true, j.ID
		} else if j.Status == store.JobFailed && d.PanelDomain == "" {
			d.PanelFailure = &j
		}
	}
	if j, err := s.st.LatestJob("panel-https-off"); err == nil && !j.Done() {
		d.PanelBusy, d.PanelJobID = true, j.ID
	}
	d.ClientID, _ = s.st.Setting(keyClientID)
	secret, _ := s.st.Setting(keyClientSecret)
	d.SecretSet = secret != ""
	d.Login, _ = s.st.Setting(keyGitHubLogin)
	d.Connected = s.githubToken() != ""
	s.render(w, r, "settings", http.StatusOK, pageFor(sess, "Settings", "settings", d))
}

func (s *Server) saveGitHubApp(w http.ResponseWriter, r *http.Request, _ store.Session) {
	id := strings.TrimSpace(r.FormValue("client_id"))
	secret := strings.TrimSpace(r.FormValue("client_secret"))
	if !clientIDRe.MatchString(id) {
		setFlash(w, "err", "That Client ID doesn't look right.")
		redirect(w, r, "/settings")
		return
	}
	if err := s.st.SetSetting(keyClientID, id); err != nil {
		s.serverError(w, err)
		return
	}
	if secret != "" {
		if err := s.st.SetSetting(keyClientSecret, secret); err != nil {
			s.serverError(w, err)
			return
		}
	}
	setFlash(w, "ok", "GitHub OAuth app saved. Now connect your account.")
	redirect(w, r, "/settings")
}

func (s *Server) panelJobActive() bool {
	for _, kind := range []string{"panel-https", "panel-https-off"} {
		if j, err := s.st.LatestJob(kind); err == nil && !j.Done() {
			return true
		}
	}
	return false
}

func (s *Server) enablePanelHTTPS(w http.ResponseWriter, r *http.Request, _ store.Session) {
	fail := func(msg string) {
		setFlash(w, "err", msg)
		redirect(w, r, "/settings#panel")
	}
	domain := strings.ToLower(strings.TrimSpace(r.FormValue("domain")))
	email := strings.TrimSpace(r.FormValue("email"))
	if !validDomain(domain) {
		fail("Enter a domain like panel.example.com.")
		return
	}
	if ok, why := laravel.HTTPSEligible(domain); !ok {
		fail(why)
		return
	}
	if email != "" && !emailRe.MatchString(email) {
		fail("That email address doesn't look right.")
		return
	}
	if !s.provisioned() {
		fail("Provision the server first; the panel's HTTPS runs through Nginx.")
		return
	}
	if s.panelHTTPSDomain() != "" {
		fail("Remove the current panel domain first.")
		return
	}
	if s.panelJobActive() {
		fail("A panel HTTPS job is already running.")
		return
	}
	sites, _ := s.st.Sites()
	for _, x := range sites {
		if x.Domain == domain {
			fail(domain + " is already used by a site. Pick another name, e.g. panel." + domain + ".")
			return
		}
	}
	if err := s.st.SetSetting(keyACMEEmail, email); err != nil {
		s.serverError(w, err)
		return
	}
	port := s.panelPort()
	id, err := s.runner.Enqueue(nil, "panel-https", "Serve the panel at https://"+domain,
		func(ctx context.Context, log io.Writer) error {
			return s.deployer.EnablePanelHTTPS(ctx, log, domain, port, email)
		},
		func(err error) {
			if err != nil {
				return
			}
			s.panelMu.Lock()
			s.panelLink = "https://" + domain + "/settings"
			s.panelMu.Unlock()
			// Switch a moment later, so the browser watching this job over
			// plain HTTP receives the final log and the link to follow.
			time.AfterFunc(3*time.Second, func() {
				if err := s.setPanelHTTPSDomain(domain); err != nil {
					slog.Error("save panel domain", "err", err)
				}
			})
		})
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(id, 10))
}

func (s *Server) disablePanelHTTPS(w http.ResponseWriter, r *http.Request, _ store.Session) {
	domain := s.panelHTTPSDomain()
	if domain == "" {
		redirect(w, r, "/settings#panel")
		return
	}
	if s.panelJobActive() {
		setFlash(w, "err", "A panel HTTPS job is already running.")
		redirect(w, r, "/settings#panel")
		return
	}
	id, err := s.runner.Enqueue(nil, "panel-https-off", "Stop serving the panel at https://"+domain,
		func(ctx context.Context, log io.Writer) error {
			// Stop redirecting first so the panel is reachable on its port
			// again as soon as the proxy goes away.
			if err := s.setPanelHTTPSDomain(""); err != nil {
				return err
			}
			err := s.deployer.DisablePanelHTTPS(ctx, log, domain)
			if err == nil {
				fmt.Fprintf(log, "\nThe panel is reachable again at http://<server-ip>:%s\n", s.panelPort())
			}
			return err
		}, nil)
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(id, 10))
}

func (s *Server) githubConnect(w http.ResponseWriter, r *http.Request, _ store.Session) {
	id, _ := s.st.Setting(keyClientID)
	secret, _ := s.st.Setting(keyClientSecret)
	if id == "" || secret == "" {
		setFlash(w, "err", "Add your GitHub OAuth app's Client ID and secret first.")
		redirect(w, r, "/settings")
		return
	}
	state := randomHex(16)
	http.SetCookie(w, &http.Cookie{
		Name: "skiff_oauth_state", Value: state, Path: "/github/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies(r), MaxAge: 600,
	})
	http.Redirect(w, r, github.AuthorizeURL(id, s.baseURL(r)+"/github/callback", state), http.StatusFound)
}

func (s *Server) githubCallback(w http.ResponseWriter, r *http.Request, _ store.Session) {
	q := r.URL.Query()
	http.SetCookie(w, &http.Cookie{Name: "skiff_oauth_state", Path: "/github/", MaxAge: -1})
	if e := q.Get("error"); e != "" {
		setFlash(w, "err", "GitHub did not authorize Skiff: "+q.Get("error_description"))
		redirect(w, r, "/settings")
		return
	}
	c, err := r.Cookie("skiff_oauth_state")
	if err != nil || c.Value == "" || q.Get("state") != c.Value {
		setFlash(w, "err", "GitHub sign-in expired or was tampered with. Try connecting again.")
		redirect(w, r, "/settings")
		return
	}
	id, _ := s.st.Setting(keyClientID)
	secret, _ := s.st.Setting(keyClientSecret)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	token, err := github.Exchange(ctx, id, secret, q.Get("code"), s.baseURL(r)+"/github/callback")
	if err != nil {
		setFlash(w, "err", err.Error())
		redirect(w, r, "/settings")
		return
	}
	u, err := github.CurrentUser(ctx, token)
	if err != nil {
		setFlash(w, "err", "Connected, but could not read your GitHub profile: "+err.Error())
		redirect(w, r, "/settings")
		return
	}
	if err := s.st.SetSetting(keyGitHubToken, token); err != nil {
		s.serverError(w, err)
		return
	}
	_ = s.st.SetSetting(keyGitHubLogin, u.Login)
	s.forgetRepos()
	setFlash(w, "ok", "Connected to GitHub as @"+u.Login+".")
	redirect(w, r, "/repos")
}

func (s *Server) githubDisconnect(w http.ResponseWriter, r *http.Request, _ store.Session) {
	if err := s.st.DeleteSettings(keyGitHubToken, keyGitHubLogin); err != nil {
		s.serverError(w, err)
		return
	}
	s.forgetRepos()
	setFlash(w, "ok", "Disconnected from GitHub. You can also revoke Skiff in your GitHub settings.")
	redirect(w, r, "/settings")
}
