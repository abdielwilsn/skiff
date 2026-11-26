package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpgradeFromV01(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	// A v0.1 database: original schema, user_version never set.
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO sites(domain, repo, branch, php_version, deploy_script, status, created_at)
		VALUES('old.example.com','o/r','main','8.3','true','active',1)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	sites, err := s.Sites()
	if err != nil || len(sites) != 1 || sites[0].HTTPS {
		t.Fatalf("after upgrade: %+v %v", sites, err)
	}
	if err := s.SetSiteHTTPS(sites[0].ID, true); err != nil {
		t.Fatal(err)
	}
	s.Close()
	// Re-opening must not re-run migrations.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if x, _ := s2.Site(sites[0].ID); !x.HTTPS || x.Scheme() != "https" {
		t.Fatalf("https flag lost: %+v", x)
	}
}

func TestDatabases(t *testing.T) {
	s := open(t)
	siteID, _ := s.CreateSite(Site{Domain: "a.test", Repo: "o/r", Branch: "main", PHPVersion: "8.3", DeployScript: "true"})
	// Built at runtime so secret scanners don't mistake it for a credential.
	fake := strings.Repeat("x", 20)
	id, err := s.CreateDatabase(Database{Name: "a_test", Username: "a_test", Password: fake, Status: DBPending, SiteID: &siteID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDatabase(Database{Name: "a_test", Username: "x", Password: fake, Status: DBReady}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate name: %v", err)
	}
	if taken, _ := s.NameTaken("a_test"); !taken {
		t.Fatal("NameTaken should see the database")
	}
	db, err := s.DatabaseForSite(siteID)
	if err != nil || db.ID != id || db.SiteDomain != "a.test" {
		t.Fatalf("DatabaseForSite = %+v, %v", db, err)
	}

	// A pending database waiting for its site's first deploy survives a restart.
	_ = s.FailInterrupted()
	if db, _ = s.Database(id); db.Status != DBPending {
		t.Fatalf("pending attached database became %s", db.Status)
	}

	// Deleting the site keeps the database (and its data) but detaches it.
	if err := s.DeleteSite(siteID); err != nil {
		t.Fatal(err)
	}
	db, _ = s.Database(id)
	if db.SiteID != nil || db.SiteDomain != "" {
		t.Fatalf("database still attached after site deletion: %+v", db)
	}
	// Now unattached and pending: an interrupted standalone create is marked failed.
	_ = s.FailInterrupted()
	if db, _ = s.Database(id); db.Status != DBFailed {
		t.Fatalf("unattached pending database should be failed, got %s", db.Status)
	}
}

func TestUpgradeDeployScripts(t *testing.T) {
	s := open(t)
	oldDefault, edited, current := "old default\n", "old default\nmy extra step\n", "new default\n"
	a, _ := s.CreateSite(Site{Domain: "a.test", Repo: "o/r", Branch: "main", PHPVersion: "8.4", NodeVersion: "24", DeployScript: oldDefault})
	b, _ := s.CreateSite(Site{Domain: "b.test", Repo: "o/r", Branch: "main", PHPVersion: "8.4", DeployScript: edited})
	n, err := s.UpgradeDeployScripts([]string{oldDefault}, current)
	if err != nil || n != 1 {
		t.Fatalf("UpgradeDeployScripts = %d, %v", n, err)
	}
	if x, _ := s.Site(a); x.DeployScript != current || x.NodeVersion != "24" {
		t.Errorf("untouched default not upgraded: %+v", x)
	}
	if x, _ := s.Site(b); x.DeployScript != edited {
		t.Error("an edited deploy script was overwritten")
	}
}

func TestSessions(t *testing.T) {
	s := open(t)
	id, err := s.CreateUser("admin", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser("admin", "x"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate user: got %v", err)
	}
	tok, err := s.CreateSession(id, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	se, err := s.Session(tok)
	if err != nil || se.Username != "admin" || se.CSRF == "" {
		t.Fatalf("Session = %+v, %v", se, err)
	}
	if _, err := s.Session("wrong"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad token: got %v", err)
	}
	expired, _ := s.CreateSession(id, -time.Minute)
	if _, err := s.Session(expired); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session accepted: %v", err)
	}
}

func TestSitesAndJobs(t *testing.T) {
	s := open(t)
	id, err := s.CreateSite(Site{Domain: "a.test", Repo: "o/r", Branch: "main", PHPVersion: "8.3", DeployScript: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSite(Site{Domain: "a.test", Repo: "o/r", Branch: "main", PHPVersion: "8.3"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate domain: got %v", err)
	}
	jobID, err := s.CreateJob(&id, "deploy", "Deploy a.test", "")
	if err != nil {
		t.Fatal(err)
	}
	if busy, _ := s.HasActiveJob(id); !busy {
		t.Fatal("queued job should count as active")
	}
	_ = s.SetSiteStatus(id, SiteDeploying)
	if err := s.FailInterrupted(); err != nil {
		t.Fatal(err)
	}
	j, _ := s.Job(jobID)
	site, _ := s.Site(id)
	if j.Status != JobFailed || site.Status != SiteFailed {
		t.Fatalf("after FailInterrupted: job=%s site=%s", j.Status, site.Status)
	}
	if err := s.DeleteSite(id); err != nil {
		t.Fatal(err)
	}
	j, _ = s.Job(jobID)
	if j.SiteID != nil {
		t.Fatal("job should outlive its site with site_id NULL")
	}
}

func TestIdleSessionsEnd(t *testing.T) {
	s := open(t)
	id, _ := s.CreateUser("admin", "hash")
	tok, _ := s.CreateSession(id, 30*24*time.Hour)
	idleFor := func(d time.Duration) {
		s.db.Exec(`UPDATE sessions SET last_seen = ? WHERE token = ?`, time.Now().Add(-d).Unix(), hashToken(tok))
	}
	idleFor(11 * time.Hour)
	if _, err := s.Session(tok); err != nil {
		t.Fatalf("used 11h ago: %v", err)
	}
	var seen int64
	s.db.QueryRow(`SELECT last_seen FROM sessions WHERE token = ?`, hashToken(tok)).Scan(&seen)
	if time.Since(time.Unix(seen, 0)) > time.Minute {
		t.Error("using a session should keep it alive")
	}
	idleFor(SessionIdle + time.Minute)
	if _, err := s.Session(tok); !errors.Is(err, ErrNotFound) {
		t.Fatalf("idle session accepted: %v", err)
	}
	// Sessions from before last_seen existed (0) still work, then get tracked.
	old, _ := s.CreateSession(id, time.Hour)
	s.db.Exec(`UPDATE sessions SET last_seen = 0 WHERE token = ?`, hashToken(old))
	if _, err := s.Session(old); err != nil {
		t.Fatalf("pre-upgrade session: %v", err)
	}
}
