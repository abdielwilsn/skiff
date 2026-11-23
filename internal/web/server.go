// Package web is Skiff's HTTP panel: server-rendered pages, no build step.
package web

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"skiff/internal/executor"
	"skiff/internal/github"
	"skiff/internal/jobs"
	"skiff/internal/laravel"
	"skiff/internal/store"
)

//go:embed templates static
var assets embed.FS

// Setting keys.
const (
	keyProvisioned  = "server_provisioned"
	keyClientID     = "github_client_id"
	keyClientSecret = "github_client_secret"
	keyGitHubToken  = "github_token"
	keyGitHubLogin  = "github_login"
	keyACMEEmail    = "letsencrypt_email"
	keyPanelDomain  = "panel_domain" // set once the panel has a working HTTPS domain
)

type Config struct {
	Store      *store.Store
	Runner     *jobs.Runner
	Exec       executor.Executor
	DataDir    string
	ListenAddr string // e.g. ":8090"; its port is what the Nginx proxy targets
	BaseURL    string // public URL of the panel; "" derives it from each request
	DryRun     bool
	Version    string

	ACMEServer   string // optional ACME directory URL (e.g. Let's Encrypt staging)
	ACMEInsecure bool   // skip TLS verification of ACMEServer (local test servers only)
}

type Server struct {
	cfg      Config
	st       *store.Store
	runner   *jobs.Runner
	deployer *laravel.Deployer
	mux      *http.ServeMux
	pages    map[string]*template.Template
	limiter  *limiter

	setupMu    sync.Mutex
	setupToken string

	repoMu    sync.Mutex
	repoToken string
	repoAt    time.Time
	repoList  []github.Repo

	panelMu     sync.Mutex
	panelDomain string    // cached keyPanelDomain
	panelAt     time.Time // when panelDomain was read
	panelLink   string    // where to continue after the panel moves to HTTPS
}

// panelHTTPSDomain returns the panel's HTTPS domain, or "". It is cached for a
// few seconds so the CLI ("skiff reset-panel-domain") takes effect without a restart.
func (s *Server) panelHTTPSDomain() string {
	s.panelMu.Lock()
	defer s.panelMu.Unlock()
	if time.Since(s.panelAt) > 5*time.Second {
		s.panelDomain, _ = s.st.Setting(keyPanelDomain)
		s.panelAt = time.Now()
	}
	return s.panelDomain
}

func (s *Server) setPanelHTTPSDomain(domain string) error {
	var err error
	if domain == "" {
		err = s.st.DeleteSettings(keyPanelDomain)
	} else {
		err = s.st.SetSetting(keyPanelDomain, domain)
	}
	s.panelMu.Lock()
	s.panelAt = time.Time{} // force a re-read
	s.panelMu.Unlock()
	return err
}

func (s *Server) panelPort() string {
	_, port, err := net.SplitHostPort(s.cfg.ListenAddr)
	if err != nil || port == "" {
		return "8090"
	}
	return port
}

// isLoopback reports whether the request reached the panel from this machine:
// through the Nginx proxy, or an SSH tunnel.
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func New(cfg Config) (*Server, error) {
	s := &Server{
		cfg:      cfg,
		st:       cfg.Store,
		runner:   cfg.Runner,
		deployer: &laravel.Deployer{Exec: cfg.Exec, ACMEServer: cfg.ACMEServer, ACMEInsecure: cfg.ACMEInsecure},
		limiter:  newLimiter(),
	}
	if err := s.loadTemplates(); err != nil {
		return nil, err
	}
	s.needsSetup() // prints the setup token on first start
	// Sites still on an untouched older default deploy script get the current one.
	if n, err := s.st.UpgradeDeployScripts(laravel.PreviousDefaultDeployScripts, laravel.DefaultDeployScript); err != nil {
		slog.Error("upgrade deploy scripts", "err", err)
	} else if n > 0 {
		slog.Info("upgraded sites to the new default deploy script", "sites", n)
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	m := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))

	m.HandleFunc("GET /setup", s.setupPage)
	m.HandleFunc("POST /setup", s.setupSubmit)
	m.HandleFunc("GET /login", s.loginPage)
	m.HandleFunc("POST /login", s.loginSubmit)
	m.HandleFunc("POST /logout", s.auth(s.logout))

	m.HandleFunc("GET /{$}", s.auth(s.dashboard))
	m.HandleFunc("POST /server/provision", s.auth(s.provision))
	m.HandleFunc("GET /repos", s.auth(s.reposPage))

	m.HandleFunc("GET /sites/new", s.auth(s.newSitePage))
	m.HandleFunc("POST /sites", s.auth(s.createSite))
	m.HandleFunc("GET /sites/{id}", s.auth(s.sitePage))
	m.HandleFunc("GET /sites/{id}/deployments", s.auth(s.siteDeploymentsPage))
	m.HandleFunc("GET /sites/{id}/settings", s.auth(s.siteSettingsPage))
	m.HandleFunc("POST /sites/{id}/deploy", s.auth(s.deploySite))
	m.HandleFunc("POST /sites/{id}/rollback", s.auth(s.rollback))
	m.HandleFunc("POST /sites/{id}/app", s.auth(s.saveAppSettings))
	m.HandleFunc("POST /sites/{id}/workers", s.auth(s.saveWorkers))
	m.HandleFunc("POST /sites/{id}/restart", s.auth(s.restartProcesses))
	m.HandleFunc("GET /sites/{id}/logs", s.auth(s.logsPage))
	m.HandleFunc("POST /sites/{id}/script", s.auth(s.saveScript))
	m.HandleFunc("POST /sites/{id}/php", s.auth(s.setPHP))
	m.HandleFunc("POST /sites/{id}/node", s.auth(s.setNode))
	m.HandleFunc("POST /sites/{id}/autodeploy", s.auth(s.enableAutoDeploy))
	m.HandleFunc("POST /sites/{id}/autodeploy/disable", s.auth(s.disableAutoDeploy))

	// Public: GitHub can't log in, so these are authenticated by HMAC signature.
	m.HandleFunc("POST /hooks/github/{id}", s.githubWebhook)
	m.HandleFunc("GET /sites/{id}/env", s.auth(s.envPage))
	m.HandleFunc("POST /sites/{id}/env", s.auth(s.saveEnv))
	m.HandleFunc("POST /sites/{id}/delete", s.auth(s.deleteSite))
	m.HandleFunc("GET /sites/{id}/commands", s.auth(s.commandsPage))
	m.HandleFunc("POST /sites/{id}/commands", s.auth(s.runCommand))
	m.HandleFunc("POST /sites/{id}/https", s.auth(s.enableHTTPS))
	m.HandleFunc("POST /sites/{id}/https/disable", s.auth(s.disableHTTPS))

	m.HandleFunc("GET /databases", s.auth(s.databasesPage))
	m.HandleFunc("POST /databases", s.auth(s.createDatabase))
	m.HandleFunc("POST /databases/install", s.auth(s.installMySQL))
	m.HandleFunc("POST /databases/{id}/retry", s.auth(s.retryDatabase))
	m.HandleFunc("POST /databases/{id}/attach", s.auth(s.attachDatabase))
	m.HandleFunc("POST /databases/{id}/detach", s.auth(s.detachDatabase))
	m.HandleFunc("POST /databases/{id}/delete", s.auth(s.deleteDatabase))

	m.HandleFunc("GET /jobs/{id}", s.auth(s.jobPage))
	m.HandleFunc("GET /jobs/{id}/log", s.auth(s.jobLog))

	m.HandleFunc("GET /settings", s.auth(s.settingsPage))
	m.HandleFunc("POST /settings/github", s.auth(s.saveGitHubApp))
	m.HandleFunc("POST /settings/panel", s.auth(s.enablePanelHTTPS))
	m.HandleFunc("POST /settings/panel/disable", s.auth(s.disablePanelHTTPS))
	m.HandleFunc("GET /github/connect", s.auth(s.githubConnect))
	m.HandleFunc("GET /github/callback", s.auth(s.githubCallback))
	m.HandleFunc("POST /github/disconnect", s.auth(s.githubDisconnect))
	s.mux = m
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'")
	// Once the panel has an HTTPS domain, plain-HTTP visits from outside are
	// sent there, so credentials never cross the network unencrypted. Local
	// requests (the Nginx proxy, SSH tunnels) are served as usual. 307, not
	// 308: browsers cache permanent redirects, which would break the IP
	// address if the domain is later removed.
	if s.cfg.BaseURL == "" && !isLoopback(r) {
		if domain := s.panelHTTPSDomain(); domain != "" {
			http.Redirect(w, r, "https://"+domain+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			return
		}
	}
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	}
	s.mux.ServeHTTP(w, r)
}

// ---- first-run setup -------------------------------------------------------

func (s *Server) setupTokenPath() string { return filepath.Join(s.cfg.DataDir, "setup-token") }

// needsSetup reports whether no admin exists yet, creating a one-time setup
// token if needed. The token stops a stranger who finds the fresh panel first
// from claiming it.
func (s *Server) needsSetup() bool {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.setupToken != "" {
		// Follow the file in case "skiff reset-admin" replaced the token.
		if b, err := os.ReadFile(s.setupTokenPath()); err == nil && len(strings.TrimSpace(string(b))) >= 16 {
			s.setupToken = strings.TrimSpace(string(b))
		}
		return true
	}
	n, err := s.st.UserCount()
	if err != nil || n > 0 {
		return false
	}
	// Reuse a token already on disk (e.g. written by "skiff reset-admin").
	if b, err := os.ReadFile(s.setupTokenPath()); err == nil && len(strings.TrimSpace(string(b))) >= 16 {
		s.setupToken = strings.TrimSpace(string(b))
	} else {
		s.setupToken = randomHex(16)
		if err := os.WriteFile(s.setupTokenPath(), []byte(s.setupToken+"\n"), 0o600); err != nil {
			slog.Error("write setup token", "err", err)
		}
	}
	slog.Info("no admin account yet: open /setup?token="+s.setupToken+" to create one", "token_file", s.setupTokenPath())
	return true
}

// ---- rendering -------------------------------------------------------------

type flash struct{ Kind, Msg string }

type page struct {
	Host  string // the panel's host name, shown in the header
	Title string
	Nav   string // global section: sites, repos, databases, settings
	// Site pages switch the sidebar to the site's own navigation.
	Site    *store.Site
	SiteNav string // overview, deployments, env, commands, logs, settings
	Crumb   string // last breadcrumb on site pages ("" on the overview)
	User    string
	CSRF    string
	Flash   flash
	DryRun  bool
	Version string
	Data    any
}

func (s *Server) loadTemplates() error {
	funcs := template.FuncMap{
		"ago":         ago,
		"initial":     initial,
		"statusLabel": func(s string) string { return statusOf(s).label },
		"statusTone":  func(s string) string { return statusOf(s).tone },
		"statusPulse": func(s string) bool { return statusOf(s).pulse },
		// dict builds a map for passing several values to a partial.
		"dict": func(kv ...any) map[string]any {
			m := make(map[string]any, len(kv)/2)
			for i := 0; i+1 < len(kv); i += 2 {
				if k, ok := kv[i].(string); ok {
					m[k] = kv[i+1]
				}
			}
			return m
		},
	}
	s.pages = map[string]*template.Template{}
	names, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return err
	}
	for _, n := range names {
		name := strings.TrimSuffix(filepath.Base(n), ".html")
		if name == "layout" {
			continue
		}
		t, err := template.New(name).Funcs(funcs).ParseFS(assets, "templates/layout.html", n)
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return nil
}

func pageFor(sess store.Session, title, nav string, data any) page {
	return page{Title: title, Nav: nav, User: sess.Username, CSRF: sess.CSRF, Data: data}
}

// sitePageFor builds a page that belongs to one site: the sidebar shows the
// site's sections and the top bar its breadcrumbs.
func sitePageFor(sess store.Session, site store.Site, nav, crumb string, data any) page {
	title := site.Domain
	if crumb != "" {
		title += " · " + crumb
	}
	p := pageFor(sess, title, "sites", data)
	p.Site, p.SiteNav, p.Crumb = &site, nav, crumb
	return p
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, status int, p page) {
	if c, err := r.Cookie("skiff_flash"); err == nil {
		if v, err := url.QueryUnescape(c.Value); err == nil && p.Flash.Msg == "" {
			kind, msg, _ := strings.Cut(v, ":")
			p.Flash = flash{Kind: kind, Msg: msg}
		}
		http.SetCookie(w, &http.Cookie{Name: "skiff_flash", Path: "/", MaxAge: -1})
	}
	p.DryRun = s.cfg.DryRun
	p.Host = r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		p.Host = h
	}
	p.Version = s.cfg.Version
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "unknown page "+name, http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", p); err != nil {
		slog.Error("render", "page", name, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func setFlash(w http.ResponseWriter, kind, msg string) {
	http.SetCookie(w, &http.Cookie{
		Name: "skiff_flash", Value: url.QueryEscape(kind + ":" + msg),
		Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 60,
	})
}

func redirect(w http.ResponseWriter, r *http.Request, to string) {
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

type statusStyle struct {
	label, tone string // tone: ok | info | warn | err | neutral
	pulse       bool
}

// statusOf maps site, job and database statuses to how they're shown.
func statusOf(s string) statusStyle {
	switch s {
	case store.SiteActive, store.DBReady:
		return statusStyle{"Ready", "ok", false}
	case store.JobSuccess:
		return statusStyle{"Succeeded", "ok", false}
	case store.SiteDeploying:
		return statusStyle{"Building", "info", true}
	case store.JobRunning:
		return statusStyle{"Running", "info", true}
	case store.JobQueued:
		return statusStyle{"Queued", "neutral", false}
	case store.DBPending:
		return statusStyle{"Pending", "neutral", false}
	case store.SiteNew:
		return statusStyle{"Not deployed", "neutral", false}
	case store.SiteDeleting:
		return statusStyle{"Deleting", "warn", true}
	case store.JobFailed:
		return statusStyle{"Failed", "err", false}
	}
	return statusStyle{s, "neutral", false}
}

// initial returns the first letter or digit of s, for monogram avatars.
func initial(s string) string {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return strings.ToUpper(string(r))
		}
	}
	return "?"
}

func ago(v any) string {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x == nil {
			return "never"
		}
		t = *x
	default:
		return ""
	}
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2, 2006")
	}
}

// baseURL is the panel's public origin, used for the GitHub callback URL.
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return strings.TrimRight(s.cfg.BaseURL, "/")
	}
	if domain := s.panelHTTPSDomain(); domain != "" {
		return "https://" + domain
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) secureCookies(r *http.Request) bool {
	return r.TLS != nil || strings.HasPrefix(s.baseURL(r), "https://")
}
