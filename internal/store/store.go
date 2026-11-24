// Package store persists Skiff's state in a single SQLite file.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps the binary static, no cgo
)

var (
	ErrNotFound  = errors.New("not found")
	ErrDuplicate = errors.New("already exists")
)

// Site statuses.
const (
	SiteNew       = "new"
	SiteDeploying = "deploying"
	SiteActive    = "active"
	SiteFailed    = "failed"
	SiteDeleting  = "deleting"
)

// Job statuses.
const (
	JobQueued  = "queued"
	JobRunning = "running"
	JobSuccess = "success"
	JobFailed  = "failed"
)

// migrations run in order; PRAGMA user_version records how many have been
// applied. Only ever append to this list.
var migrations = []string{
	schemaV1,
	`ALTER TABLE sites ADD COLUMN https INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE databases (
		id         INTEGER PRIMARY KEY,
		name       TEXT NOT NULL UNIQUE,
		username   TEXT NOT NULL UNIQUE,
		password   TEXT NOT NULL,
		status     TEXT NOT NULL,
		site_id    INTEGER REFERENCES sites(id) ON DELETE SET NULL,
		created_at INTEGER NOT NULL
	)`,
	`ALTER TABLE sites ADD COLUMN node_version TEXT NOT NULL DEFAULT '24'`,
	`ALTER TABLE sites ADD COLUMN auto_deploy INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sites ADD COLUMN hook_secret TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sites ADD COLUMN hook_id INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN detail TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sites ADD COLUMN kind TEXT NOT NULL DEFAULT 'laravel'`,
	`ALTER TABLE sites ADD COLUMN start_command TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sites ADD COLUMN port INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sites ADD COLUMN output_dir TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sites ADD COLUMN worker_count INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sites ADD COLUMN worker_options TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sites ADD COLUMN scheduler INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sites ADD COLUMN python_version TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE backups (
		id          INTEGER PRIMARY KEY,
		source      TEXT NOT NULL,
		label       TEXT NOT NULL,
		site_id     INTEGER REFERENCES sites(id) ON DELETE SET NULL,
		reason      TEXT NOT NULL,
		path        TEXT NOT NULL,
		size        INTEGER NOT NULL DEFAULT 0,
		remote      TEXT NOT NULL DEFAULT '',
		status      TEXT NOT NULL,
		error       TEXT NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL
	)`,
	`CREATE INDEX backups_source ON backups(source, created_at)`,
	`ALTER TABLE sites ADD COLUMN environment TEXT NOT NULL DEFAULT 'production'`,
	`ALTER TABLE sites ADD COLUMN parent_id INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE databases ADD COLUMN engine TEXT NOT NULL DEFAULT 'mysql'`,
	`ALTER TABLE sites ADD COLUMN redis INTEGER NOT NULL DEFAULT 0`,
}

const schemaV1 = `
CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY,
	username      TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token      TEXT PRIMARY KEY,      -- sha256 of the cookie value
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	csrf       TEXT NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sites (
	id            INTEGER PRIMARY KEY,
	domain        TEXT NOT NULL UNIQUE,
	repo          TEXT NOT NULL,
	branch        TEXT NOT NULL,
	php_version   TEXT NOT NULL,
	deploy_script TEXT NOT NULL,
	status        TEXT NOT NULL,
	created_at    INTEGER NOT NULL,
	deployed_at   INTEGER
);
CREATE TABLE IF NOT EXISTS jobs (
	id          INTEGER PRIMARY KEY,
	site_id     INTEGER REFERENCES sites(id) ON DELETE SET NULL,
	kind        TEXT NOT NULL,
	title       TEXT NOT NULL,
	status      TEXT NOT NULL,
	created_at  INTEGER NOT NULL,
	started_at  INTEGER,
	finished_at INTEGER
);
CREATE INDEX IF NOT EXISTS jobs_by_site ON jobs(site_id, id DESC);
`

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection is plenty for a single-admin panel and avoids SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() int64 { return time.Now().Unix() }

func optTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0)
	return &t
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ---- users & sessions ----------------------------------------------------

type User struct {
	ID           int64
	Username     string
	PasswordHash string
}

type Session struct {
	UserID   int64
	Username string
	CSRF     string
}

func (s *Store) UserCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) CreateUser(username, passwordHash string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO users(username, password_hash, created_at) VALUES(?,?,?)`,
		username, passwordHash, now())
	if isUnique(err) {
		return 0, ErrDuplicate
	}
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UserByName(username string) (User, error) {
	var u User
	err := s.db.QueryRow(`SELECT id, username, password_hash FROM users WHERE username = ?`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// ResetUsers removes every account and session, so the setup flow runs again.
func (s *Store) ResetUsers() error {
	if _, err := s.db.Exec(`DELETE FROM sessions`); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM users`)
	return err
}

// CreateSession returns the raw token to put in the cookie; only its hash is stored.
func (s *Store) CreateSession(userID int64, ttl time.Duration) (string, error) {
	token := randomHex(32)
	_, err := s.db.Exec(`INSERT INTO sessions(token, user_id, csrf, expires_at) VALUES(?,?,?,?)`,
		hashToken(token), userID, randomHex(32), time.Now().Add(ttl).Unix())
	if err != nil {
		return "", err
	}
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, now())
	return token, nil
}

func (s *Store) Session(token string) (Session, error) {
	var se Session
	err := s.db.QueryRow(`
		SELECT s.user_id, u.username, s.csrf
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token = ? AND s.expires_at > ?`, hashToken(token), now()).
		Scan(&se.UserID, &se.Username, &se.CSRF)
	if errors.Is(err, sql.ErrNoRows) {
		return se, ErrNotFound
	}
	return se, err
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, hashToken(token))
	return err
}

// ---- settings --------------------------------------------------------------

// Setting returns "" when the key is not set.
func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key, value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *Store) DeleteSettings(keys ...string) error {
	for _, k := range keys {
		if _, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, k); err != nil {
			return err
		}
	}
	return nil
}

// ---- sites -----------------------------------------------------------------

// Site kinds.
const (
	KindLaravel = "laravel"
	KindNode    = "node"   // a Node.js server (Next.js, Nuxt, Express…) behind Nginx
	KindStatic  = "static" // built files served by Nginx (Vite SPA, Astro…)
	KindPython  = "python" // a Python app server (Django, FastAPI, Flask…) behind Nginx
)

// EnvProduction is the environment of every site that isn't another site's
// staging (or other) environment.
const EnvProduction = "production"

// AppPortBase + site ID is the local port a Node or Python app listens on.
const AppPortBase = 4100

type Site struct {
	ID           int64
	Domain       string
	Repo         string // GitHub "owner/name"
	Branch       string
	PHPVersion   string
	DeployScript string
	Status       string
	NodeVersion  string // Node.js major for asset builds; "" = none
	AutoDeploy   bool   // deploy on every push to Branch (GitHub webhook)
	HookSecret   string // HMAC secret GitHub signs webhook payloads with
	HookID       int64  // GitHub's id for the webhook, to remove it later
	HTTPS        bool   // a Let's Encrypt certificate is installed

	Kind          string // KindLaravel, KindNode, KindStatic or KindPython
	StartCommand  string // Node/Python apps: how to start the server, e.g. "npm run start"
	Port          int    // Node/Python apps: local port the app listens on
	OutputDir     string // static sites: build output served by Nginx, e.g. "dist"
	WorkerCount   int    // Laravel: queue:work processes to keep running
	WorkerOptions string // Laravel: options for queue:work
	Scheduler     bool   // Laravel: run schedule:run every minute
	PythonVersion string // Python apps: interpreter version, e.g. "3.13"

	// Environments: a production site (ParentID 0) can have others, such as
	// staging, that deploy the same repository from their own branch to their
	// own domain, with their own .env and database.
	Environment string // "production", "staging", …
	ParentID    int64  // the production site; 0 for a production site

	Redis bool // the site's .env points at the server's Redis

	CreatedAt  time.Time
	DeployedAt *time.Time
}

func (x Site) IsLaravel() bool { return x.Kind == "" || x.Kind == KindLaravel }
func (x Site) IsNode() bool    { return x.Kind == KindNode }
func (x Site) IsStatic() bool  { return x.Kind == KindStatic }
func (x Site) IsPython() bool  { return x.Kind == KindPython }

// IsProduction reports whether this is a production site rather than one of
// its other environments.
func (x Site) IsProduction() bool { return x.ParentID == 0 }

// RootID is the id of the production site this site belongs to.
func (x Site) RootID() int64 {
	if x.ParentID != 0 {
		return x.ParentID
	}
	return x.ID
}

// EnvLabel is the environment's name for headings, e.g. "Staging".
func (x Site) EnvLabel() string {
	e := x.Environment
	if e == "" {
		e = EnvProduction
	}
	return strings.ToUpper(e[:1]) + e[1:]
}

// EnvName is the environment's name, e.g. "staging".
func (x Site) EnvName() string {
	if x.Environment == "" {
		return EnvProduction
	}
	return x.Environment
}

// HasApp reports whether the site runs its own server process (Node or
// Python) that Nginx proxies to.
func (x Site) HasApp() bool { return x.IsNode() || x.IsPython() }

// KindLabel is the human name of the site's project type.
func (x Site) KindLabel() string {
	switch x.Kind {
	case KindNode:
		return "Node.js app"
	case KindStatic:
		return "Static site"
	case KindPython:
		return "Python app"
	}
	return "Laravel"
}

// Scheme is "https" once a certificate is installed, else "http".
func (x Site) Scheme() string {
	if x.HTTPS {
		return "https"
	}
	return "http"
}

const siteCols = `id, domain, repo, branch, php_version, node_version, auto_deploy, hook_secret, hook_id, deploy_script, status, https, kind, start_command, port, output_dir, worker_count, worker_options, scheduler, python_version, environment, parent_id, redis, created_at, deployed_at`

type scanner interface{ Scan(dest ...any) error }

func scanSite(row scanner) (Site, error) {
	var x Site
	var created int64
	var deployed sql.NullInt64
	err := row.Scan(&x.ID, &x.Domain, &x.Repo, &x.Branch, &x.PHPVersion, &x.NodeVersion, &x.AutoDeploy, &x.HookSecret, &x.HookID, &x.DeployScript, &x.Status, &x.HTTPS,
		&x.Kind, &x.StartCommand, &x.Port, &x.OutputDir, &x.WorkerCount, &x.WorkerOptions, &x.Scheduler, &x.PythonVersion, &x.Environment, &x.ParentID, &x.Redis, &created, &deployed)
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	x.CreatedAt = time.Unix(created, 0)
	x.DeployedAt = optTime(deployed)
	return x, err
}

func (s *Store) CreateSite(x Site) (int64, error) {
	if x.Kind == "" {
		x.Kind = KindLaravel
	}
	if x.Environment == "" {
		x.Environment = EnvProduction
	}
	res, err := s.db.Exec(`INSERT INTO sites(domain, repo, branch, php_version, node_version, deploy_script, status, kind, start_command, output_dir, python_version,
		worker_count, worker_options, scheduler, environment, parent_id, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, x.Domain, x.Repo, x.Branch, x.PHPVersion, x.NodeVersion, x.DeployScript, SiteNew,
		x.Kind, x.StartCommand, x.OutputDir, x.PythonVersion, x.WorkerCount, x.WorkerOptions, x.Scheduler, x.Environment, x.ParentID, now())
	if isUnique(err) {
		return 0, ErrDuplicate
	}
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if x.HasApp() {
		// A stable, unique local port per site.
		if _, err := s.db.Exec(`UPDATE sites SET port = ? WHERE id = ?`, AppPortBase+id, id); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// SetSitePython sets a Python app's interpreter version.
func (s *Store) SetSitePython(id int64, version string) error {
	_, err := s.db.Exec(`UPDATE sites SET python_version = ? WHERE id = ?`, version, id)
	return err
}

// SetAppSettings updates a Node app's start command or a static site's output folder.
func (s *Store) SetAppSettings(id int64, startCommand, outputDir string) error {
	_, err := s.db.Exec(`UPDATE sites SET start_command = ?, output_dir = ? WHERE id = ?`, startCommand, outputDir, id)
	return err
}

// SetWorkers sets a Laravel site's queue workers and scheduler.
func (s *Store) SetWorkers(id int64, count int, options string, scheduler bool) error {
	_, err := s.db.Exec(`UPDATE sites SET worker_count = ?, worker_options = ?, scheduler = ? WHERE id = ?`, count, options, scheduler, id)
	return err
}

func (s *Store) Site(id int64) (Site, error) {
	return scanSite(s.db.QueryRow(`SELECT `+siteCols+` FROM sites WHERE id = ?`, id))
}

func (s *Store) Sites() ([]Site, error) {
	rows, err := s.db.Query(`SELECT ` + siteCols + ` FROM sites ORDER BY domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Site
	for rows.Next() {
		x, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Environments returns every environment of the site's project: the
// production site first, then the others by name.
func (s *Store) Environments(site Site) ([]Site, error) {
	root := site.RootID()
	rows, err := s.db.Query(`SELECT `+siteCols+` FROM sites WHERE id = ? OR parent_id = ? ORDER BY parent_id <> 0, environment, domain`, root, root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Site
	for rows.Next() {
		x, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ChildCount is how many other environments a production site has.
func (s *Store) ChildCount(id int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sites WHERE parent_id = ?`, id).Scan(&n)
	return n, err
}

// SetSiteRedis records whether a site uses the server's Redis.
func (s *Store) SetSiteRedis(id int64, on bool) error {
	_, err := s.db.Exec(`UPDATE sites SET redis = ? WHERE id = ?`, on, id)
	return err
}

func (s *Store) SetSiteStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE sites SET status = ? WHERE id = ?`, status, id)
	return err
}

func (s *Store) MarkSiteDeployed(id int64) error {
	_, err := s.db.Exec(`UPDATE sites SET status = ?, deployed_at = ? WHERE id = ?`, SiteActive, now(), id)
	return err
}

func (s *Store) SetSiteHTTPS(id int64, on bool) error {
	_, err := s.db.Exec(`UPDATE sites SET https = ? WHERE id = ?`, on, id)
	return err
}

// SetAutoDeploy turns push-to-deploy on (with the webhook's secret and id)
// or off (pass false, "", 0).
func (s *Store) SetAutoDeploy(id int64, on bool, secret string, hookID int64) error {
	_, err := s.db.Exec(`UPDATE sites SET auto_deploy = ?, hook_secret = ?, hook_id = ? WHERE id = ?`, on, secret, hookID, id)
	return err
}

// HasQueuedJob reports whether the site has a job of this kind waiting to run
// (not yet started).
func (s *Store) HasQueuedJob(siteID int64, kind string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE site_id = ? AND kind = ? AND status = ?`,
		siteID, kind, JobQueued).Scan(&n)
	return n > 0, err
}

func (s *Store) SetSiteNode(id int64, major string) error {
	_, err := s.db.Exec(`UPDATE sites SET node_version = ? WHERE id = ?`, major, id)
	return err
}

// UpgradeDeployScripts replaces deploy scripts that are still exactly one of
// Skiff's previous defaults with the current default. Edited scripts are kept.
func (s *Store) UpgradeDeployScripts(previous []string, current string) (int64, error) {
	var total int64
	for _, old := range previous {
		res, err := s.db.Exec(`UPDATE sites SET deploy_script = ? WHERE deploy_script = ?`, current, old)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

func (s *Store) SetSitePHP(id int64, php string) error {
	_, err := s.db.Exec(`UPDATE sites SET php_version = ? WHERE id = ?`, php, id)
	return err
}

func (s *Store) SetDeployScript(id int64, script string) error {
	_, err := s.db.Exec(`UPDATE sites SET deploy_script = ? WHERE id = ?`, script, id)
	return err
}

func (s *Store) DeleteSite(id int64) error {
	_, err := s.db.Exec(`DELETE FROM sites WHERE id = ?`, id)
	return err
}

// ---- databases -------------------------------------------------------------

// Database statuses.
const (
	DBPending  = "pending" // recorded; created on the server by a queued job
	DBReady    = "ready"
	DBFailed   = "failed"
	DBDeleting = "deleting"
)

// Database engines.
const (
	EngineMySQL    = "mysql"
	EnginePostgres = "postgres"
)

// Database is a MySQL or PostgreSQL database plus the user that owns it.
type Database struct {
	ID         int64
	Engine     string // EngineMySQL or EnginePostgres
	Name       string
	Username   string
	Password   string
	Status     string
	SiteID     *int64
	SiteDomain string // joined from sites; "" when not attached
	CreatedAt  time.Time
}

// EngineLabel is the engine's display name.
func (d Database) EngineLabel() string {
	if d.Engine == EnginePostgres {
		return "PostgreSQL"
	}
	return "MySQL"
}

func (d Database) IsPostgres() bool { return d.Engine == EnginePostgres }

const dbCols = `d.id, d.engine, d.name, d.username, d.password, d.status, d.site_id, COALESCE(s.domain, ''), d.created_at`
const dbFrom = ` FROM databases d LEFT JOIN sites s ON s.id = d.site_id`

func scanDatabase(row scanner) (Database, error) {
	var d Database
	var site sql.NullInt64
	var created int64
	err := row.Scan(&d.ID, &d.Engine, &d.Name, &d.Username, &d.Password, &d.Status, &site, &d.SiteDomain, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if site.Valid {
		id := site.Int64
		d.SiteID = &id
	}
	d.CreatedAt = time.Unix(created, 0)
	return d, err
}

func (s *Store) CreateDatabase(d Database) (int64, error) {
	if d.Engine == "" {
		d.Engine = EngineMySQL
	}
	res, err := s.db.Exec(`INSERT INTO databases(engine, name, username, password, status, site_id, created_at) VALUES(?,?,?,?,?,?,?)`,
		d.Engine, d.Name, d.Username, d.Password, d.Status, d.SiteID, now())
	if isUnique(err) {
		return 0, ErrDuplicate
	}
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Database(id int64) (Database, error) {
	return scanDatabase(s.db.QueryRow(`SELECT `+dbCols+dbFrom+` WHERE d.id = ?`, id))
}

// DatabaseForSite returns the database attached to a site, or ErrNotFound.
func (s *Store) DatabaseForSite(siteID int64) (Database, error) {
	return scanDatabase(s.db.QueryRow(`SELECT `+dbCols+dbFrom+` WHERE d.site_id = ? ORDER BY d.id LIMIT 1`, siteID))
}

func (s *Store) Databases() ([]Database, error) {
	rows, err := s.db.Query(`SELECT ` + dbCols + dbFrom + ` ORDER BY d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Database
	for rows.Next() {
		d, err := scanDatabase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// NameTaken reports whether a database or user name is already used.
func (s *Store) NameTaken(name string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM databases WHERE name = ? OR username = ?`, name, name).Scan(&n)
	return n > 0, err
}

func (s *Store) SetDatabaseStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE databases SET status = ? WHERE id = ?`, status, id)
	return err
}

// AttachDatabase links a database to a site (nil detaches it).
func (s *Store) AttachDatabase(id int64, siteID *int64) error {
	_, err := s.db.Exec(`UPDATE databases SET site_id = ? WHERE id = ?`, siteID, id)
	return err
}

func (s *Store) DeleteDatabase(id int64) error {
	_, err := s.db.Exec(`DELETE FROM databases WHERE id = ?`, id)
	return err
}

// ---- jobs ------------------------------------------------------------------

type Job struct {
	ID         int64
	SiteID     *int64
	Kind       string
	Title      string
	Detail     string // e.g. the full command a "command" job ran
	Status     string
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

func (j Job) Done() bool { return j.Status == JobSuccess || j.Status == JobFailed }

// Duration is how long the job ran (so far), rounded for display.
func (j Job) Duration() string {
	if j.StartedAt == nil {
		return ""
	}
	end := time.Now()
	if j.FinishedAt != nil {
		end = *j.FinishedAt
	}
	return end.Sub(*j.StartedAt).Round(time.Second).String()
}

const jobCols = `id, site_id, kind, title, detail, status, created_at, started_at, finished_at`

func scanJob(row scanner) (Job, error) {
	var j Job
	var site, started, finished sql.NullInt64
	var created int64
	err := row.Scan(&j.ID, &site, &j.Kind, &j.Title, &j.Detail, &j.Status, &created, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if site.Valid {
		id := site.Int64
		j.SiteID = &id
	}
	j.CreatedAt = time.Unix(created, 0)
	j.StartedAt = optTime(started)
	j.FinishedAt = optTime(finished)
	return j, err
}

func (s *Store) queryJobs(q string, args ...any) ([]Job, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) CreateJob(siteID *int64, kind, title, detail string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO jobs(site_id, kind, title, detail, status, created_at) VALUES(?,?,?,?,?,?)`,
		siteID, kind, title, detail, JobQueued, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Job(id int64) (Job, error) {
	return scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
}

func (s *Store) RecentJobs(limit int) ([]Job, error) {
	return s.queryJobs(`SELECT `+jobCols+` FROM jobs ORDER BY id DESC LIMIT ?`, limit)
}

// SiteJobsOfKind lists a site's most recent jobs of one kind, e.g. "command".
func (s *Store) SiteJobsOfKind(siteID int64, kind string, limit int) ([]Job, error) {
	return s.queryJobs(`SELECT `+jobCols+` FROM jobs WHERE site_id = ? AND kind = ? ORDER BY id DESC LIMIT ?`, siteID, kind, limit)
}

func (s *Store) SiteJobs(siteID int64, limit int) ([]Job, error) {
	return s.queryJobs(`SELECT `+jobCols+` FROM jobs WHERE site_id = ? ORDER BY id DESC LIMIT ?`, siteID, limit)
}

// LatestJob returns the most recent job of a kind, or ErrNotFound.
func (s *Store) LatestJob(kind string) (Job, error) {
	return scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE kind = ? ORDER BY id DESC LIMIT 1`, kind))
}

// HasActiveJob reports whether the site has a queued or running job.
func (s *Store) HasActiveJob(siteID int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE site_id = ? AND status IN (?, ?)`,
		siteID, JobQueued, JobRunning).Scan(&n)
	return n > 0, err
}

func (s *Store) SetJobStatus(id int64, status string) error {
	var err error
	switch status {
	case JobRunning:
		_, err = s.db.Exec(`UPDATE jobs SET status = ?, started_at = ? WHERE id = ?`, status, now(), id)
	case JobSuccess, JobFailed:
		_, err = s.db.Exec(`UPDATE jobs SET status = ?, finished_at = ? WHERE id = ?`, status, now(), id)
	default:
		_, err = s.db.Exec(`UPDATE jobs SET status = ? WHERE id = ?`, status, id)
	}
	return err
}

// FailInterrupted marks work that was in flight when Skiff last stopped as failed.
func (s *Store) FailInterrupted() error {
	if _, err := s.db.Exec(`UPDATE jobs SET status = ?, finished_at = ? WHERE status IN (?, ?)`,
		JobFailed, now(), JobQueued, JobRunning); err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE sites SET status = ? WHERE status IN (?, ?)`, SiteFailed, SiteDeploying, SiteDeleting); err != nil {
		return err
	}
	// A pending database attached to a never-deployed site is retried by its
	// next deploy, so leave those alone.
	if _, err := s.db.Exec(`UPDATE databases SET status = ? WHERE status = ? OR (status = ? AND site_id IS NULL)`,
		DBFailed, DBDeleting, DBPending); err != nil {
		return err
	}
	return s.FailInterruptedBackups()
}

// ---- backups ---------------------------------------------------------------

// Backup statuses.
const (
	BackupRunning = "running"
	BackupOK      = "ok"
	BackupFailed  = "failed"
)

// Backup reasons.
const (
	BackupScheduled = "scheduled"
	BackupManual    = "manual"
	BackupDeploy    = "deploy"  // taken right before a deploy
	BackupRestore   = "restore" // taken right before a restore
)

// Backup is one database dump.
type Backup struct {
	ID        int64
	Source    string // "mysql:<db>" or "sqlite:<domain>:<path in shared/>"
	Label     string // human name of the database
	SiteID    *int64
	Reason    string
	Path      string // local file
	Size      int64
	Remote    string // object key in off-site storage; "" if not uploaded
	Status    string
	Error     string
	CreatedAt time.Time
}

const backupCols = `id, source, label, site_id, reason, path, size, remote, status, error, created_at`

func scanBackup(row scanner) (Backup, error) {
	var b Backup
	var site sql.NullInt64
	var created int64
	err := row.Scan(&b.ID, &b.Source, &b.Label, &site, &b.Reason, &b.Path, &b.Size, &b.Remote, &b.Status, &b.Error, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	if site.Valid {
		id := site.Int64
		b.SiteID = &id
	}
	b.CreatedAt = time.Unix(created, 0)
	return b, err
}

func (s *Store) queryBackups(q string, args ...any) ([]Backup, error) {
	rows, err := s.db.Query(`SELECT `+backupCols+` FROM backups `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CreateBackup records a backup that is about to run.
func (s *Store) CreateBackup(b Backup) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO backups(source, label, site_id, reason, path, status, created_at) VALUES(?,?,?,?,?,?,?)`,
		b.Source, b.Label, b.SiteID, b.Reason, b.Path, BackupRunning, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishBackup stores a backup's outcome.
func (s *Store) FinishBackup(id int64, status string, size int64, errMsg string) error {
	_, err := s.db.Exec(`UPDATE backups SET status = ?, size = ?, error = ? WHERE id = ?`, status, size, errMsg, id)
	return err
}

// SetBackupRemote records where the off-site copy is ("" = none).
func (s *Store) SetBackupRemote(id int64, key string) error {
	_, err := s.db.Exec(`UPDATE backups SET remote = ? WHERE id = ?`, key, id)
	return err
}

func (s *Store) Backup(id int64) (Backup, error) {
	return scanBackup(s.db.QueryRow(`SELECT `+backupCols+` FROM backups WHERE id = ?`, id))
}

// Backups lists the most recent backups, newest first.
func (s *Store) Backups(limit int) ([]Backup, error) {
	return s.queryBackups(`ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
}

// BackupsOf lists a source's backups, newest first.
func (s *Store) BackupsOf(source string) ([]Backup, error) {
	return s.queryBackups(`WHERE source = ? ORDER BY created_at DESC, id DESC`, source)
}

// LatestBackups returns each source's newest backup.
func (s *Store) LatestBackups() (map[string]Backup, error) {
	list, err := s.queryBackups(`WHERE id IN (SELECT MAX(id) FROM backups GROUP BY source)`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Backup, len(list))
	for _, b := range list {
		out[b.Source] = b
	}
	return out, nil
}

func (s *Store) DeleteBackup(id int64) error {
	_, err := s.db.Exec(`DELETE FROM backups WHERE id = ?`, id)
	return err
}

// FailInterruptedBackups marks backups that were running when Skiff stopped.
func (s *Store) FailInterruptedBackups() error {
	_, err := s.db.Exec(`UPDATE backups SET status = ?, error = 'Interrupted (Skiff restarted)' WHERE status = ?`, BackupFailed, BackupRunning)
	return err
}
