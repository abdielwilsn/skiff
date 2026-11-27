package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"skiff/internal/executor"
	"skiff/internal/jobs"
	"skiff/internal/store"
)

// fakeServer answers the scripts backups and health checks run.
type fakeServer struct {
	mu      sync.Mutex
	scripts []string
	stdins  []string
	respond func(script string, w io.Writer) bool // optional extra answers
}

func (f *fakeServer) Run(_ context.Context, script string, opt executor.Options, w io.Writer) error {
	f.mu.Lock()
	f.scripts = append(f.scripts, script)
	if opt.Stdin != nil {
		b, _ := io.ReadAll(opt.Stdin)
		f.stdins = append(f.stdins, string(b))
	}
	respond := f.respond
	f.mu.Unlock()
	if respond != nil && respond(script, w) {
		return nil
	}
	switch {
	case strings.Contains(script, `printf 'DB\t%s\t%s\n'`) && strings.Contains(script, "app.example.com"):
		io.WriteString(w, "DB\tapp.example.com\tdatabase/database.sqlite\n")
	case strings.Contains(script, `echo "SIZE=$SIZE"`):
		io.WriteString(w, "SIZE=2048\n")
	case strings.Contains(script, "cpu_a="):
		io.WriteString(w, "cpu_a=100 50\ncpu_b=200 100\ncpus=1\nmemtotal=1000\nmemavailable=100\ndisk=100 96 4\nsvc=nginx.service active\n")
	}
	return nil
}

func (f *fakeServer) ran(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.scripts {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

func newFakeServer(t *testing.T) (*Server, *fakeServer) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "skiff.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f := &fakeServer{}
	s, err := New(Config{Store: st, Runner: jobs.NewRunner(ctx, st, dir), Exec: f, DataDir: dir, ListenAddr: ":8090", Version: "test", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	return s, f
}

// laravelSiteWithDatabases creates a deployed Laravel site with a ready MySQL
// database (its SQLite file is reported by fakeServer).
func laravelSiteWithDatabases(t *testing.T, s *Server) store.Site {
	t.Helper()
	id, err := s.st.CreateSite(store.Site{Domain: "app.example.com", Repo: "o/app", Branch: "main", PHPVersion: "8.4", Kind: store.KindLaravel, DeployScript: "true"})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.st.MarkSiteDeployed(id)
	if _, err := s.st.CreateDatabase(store.Database{Name: "app_example_com", Username: "app_example_com", Password: "Secret123pass", Status: store.DBReady, SiteID: &id}); err != nil {
		t.Fatal(err)
	}
	site, _ := s.st.Site(id)
	return site
}

func TestBackupAllAndPrune(t *testing.T) {
	s, f := newFakeServer(t)
	site := laravelSiteWithDatabases(t, s)
	for k, v := range map[string]string{keyS3Endpoint: "https://s3.example.com", keyS3Region: "us-east-1", keyS3Bucket: "bkp", keyS3AccessKey: "AK", keyS3SecretKey: "SK", keyBackupKeep: "2"} {
		_ = s.st.SetSetting(k, v)
	}
	ctx := context.Background()
	if err := s.backupAll(ctx, io.Discard, store.BackupManual); err != nil {
		t.Fatal(err)
	}
	list, _ := s.st.Backups(10)
	if len(list) != 2 {
		t.Fatalf("want a MySQL and a SQLite backup, got %+v", list)
	}
	for _, b := range list {
		if b.Status != store.BackupOK || b.Size != 2048 || b.Remote == "" || b.SiteID == nil || *b.SiteID != site.ID {
			t.Errorf("backup = %+v", b)
		}
	}
	if f.ran("mysqldump") != 1 || f.ran(`sqlite3 "$1"`) != 1 || f.ran("-T \"$FILE\"") != 2 {
		t.Error("expected one dump of each database, each uploaded")
	}

	// Keep=2 per database; safety backups (before deploys) are kept apart, 3 at most.
	src, err := s.sourceOf("mysql:app_example_com")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_ = s.runBackup(ctx, io.Discard, src, store.BackupManual)
	}
	for i := 0; i < 5; i++ {
		_ = s.runBackup(ctx, io.Discard, src, store.BackupDeploy)
	}
	var manual, deploy int
	all, _ := s.st.BackupsOf(src.ID)
	for _, b := range all {
		if b.Reason == store.BackupDeploy {
			deploy++
		} else {
			manual++
		}
	}
	if manual != 2 || deploy != 3 {
		t.Errorf("after pruning: %d manual, %d deploy backups; want 2 and 3", manual, deploy)
	}
	if f.ran("-X DELETE") == 0 || f.ran("rm -f -- '/var/backups/skiff/mysql-app_example_com/") == 0 {
		t.Error("pruned backups weren't deleted locally and off-site")
	}
}

func TestBackupBeforeDeploy(t *testing.T) {
	s, f := newFakeServer(t)
	site := laravelSiteWithDatabases(t, s)
	if err := s.backupBeforeDeploy(context.Background(), io.Discard, site); err != nil {
		t.Fatal(err)
	}
	if f.ran("mysqldump") != 1 {
		t.Error("no backup before deploy")
	}
	_ = s.st.SetSetting(keyBackupBeforeDeploy, "0")
	_ = s.backupBeforeDeploy(context.Background(), io.Discard, site)
	if f.ran("mysqldump") != 1 {
		t.Error("backed up although turned off")
	}
	static := store.Site{ID: 99, Domain: "s.example.com", Kind: store.KindStatic}
	_ = s.st.SetSetting(keyBackupBeforeDeploy, "1")
	if err := s.backupBeforeDeploy(context.Background(), io.Discard, static); err != nil || f.ran("mysqldump") != 1 {
		t.Error("static sites have nothing to back up")
	}
}

func TestNightlyBackupRunsOncePerDay(t *testing.T) {
	s, _ := newFakeServer(t)
	_ = s.st.SetSetting(keyBackupTime, "03:00")
	day := time.Date(2026, 11, 23, 2, 59, 0, 0, time.Local)

	// Freshly installed: the first check only records the day.
	s.maybeScheduleBackup(day.Add(-24 * time.Hour).Add(2 * time.Hour))
	if _, err := s.st.LatestJob("backup"); err == nil {
		t.Fatal("backed up the moment Skiff started")
	}
	// Nothing to back up: no job either.
	s.maybeScheduleBackup(day.Add(2 * time.Minute))
	if _, err := s.st.LatestJob("backup"); err == nil {
		t.Fatal("queued a backup with no databases")
	}
	laravelSiteWithDatabases(t, s)
	_ = s.st.SetSetting(keyBackupLastRun, "2026-11-22")

	s.maybeScheduleBackup(day)
	if _, err := s.st.LatestJob("backup"); err == nil {
		t.Fatal("ran before the scheduled time")
	}
	s.maybeScheduleBackup(day.Add(2 * time.Minute))
	first, err := s.st.LatestJob("backup")
	if err != nil {
		t.Fatal("didn't run at the scheduled time")
	}
	s.maybeScheduleBackup(day.Add(3 * time.Hour))
	if j, _ := s.st.LatestJob("backup"); j.ID != first.ID {
		t.Error("ran twice on the same day")
	}
	_ = s.st.SetSetting(keyBackupEnabled, "0")
	s.maybeScheduleBackup(day.Add(24 * time.Hour))
	if j, _ := s.st.LatestJob("backup"); j.ID != first.ID {
		t.Error("ran while turned off")
	}
}

func TestBackupAndServerPages(t *testing.T) {
	s, _ := newFakeServer(t)
	laravelSiteWithDatabases(t, s)
	_ = s.backupAll(context.Background(), io.Discard, store.BackupManual)
	render := func(h func(http.ResponseWriter, *http.Request, store.Session)) string {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodGet, "/", nil), store.Session{Username: "admin"})
		if w.Code != 200 {
			t.Errorf("status %d", w.Code)
		}
		return w.Body.String()
	}
	if body := render(s.backupsPage); !strings.Contains(body, "app_example_com") || !strings.Contains(body, "Add off-site storage") {
		t.Error("backups page should list the database and suggest off-site storage")
	}
	// The fake server's disk is 96% full.
	if body := render(s.serverPage); !strings.Contains(body, "96%") || !strings.Contains(body, "Critical:") || !strings.Contains(body, "nginx") {
		t.Error("server page should show disk use, a critical alert and services")
	}
	if body := render(s.dashboard); !strings.Contains(body, `class="server-strip card"`) || !strings.Contains(body, "Last backup") {
		t.Error("dashboard should show the server strip once there's a sample")
	}
}

func TestSaveBackupSettingsValidates(t *testing.T) {
	s, _ := newFakeServer(t)
	post := func(h func(http.ResponseWriter, *http.Request, store.Session), form url.Values) {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h(httptest.NewRecorder(), r, store.Session{})
	}
	post(s.saveBackupSettings, url.Values{"time": {"25:00"}, "keep": {"5"}, "enabled": {"1"}})
	if v, _ := s.st.Setting(keyBackupTime); v != "" {
		t.Errorf("invalid time saved: %q", v)
	}
	post(s.saveBackupSettings, url.Values{"time": {"04:30"}, "keep": {"5"}})
	if b := s.backupSettings(); b.Time != "04:30" || b.Keep != 5 || b.Enabled || b.BeforeDeploy {
		t.Errorf("settings = %+v", b)
	}
	post(s.saveBackupStorage, url.Values{"endpoint": {"https://x.r2.cloudflarestorage.com"}, "region": {"auto"}, "bucket": {"bkp"}, "access_key": {"AK"}, "secret_key": {`a" -o /x "`}})
	if s.s3Config().Configured() {
		t.Error("a secret with quotes was accepted")
	}
	post(s.saveBackupStorage, url.Values{"endpoint": {"https://x.r2.cloudflarestorage.com"}, "region": {"auto"}, "bucket": {"bkp"}, "access_key": {"AK"}, "secret_key": {"SK"}})
	if c := s.s3Config(); !c.Configured() || c.SecretKey != "SK" {
		t.Errorf("storage not saved: %+v", c)
	}
	// A blank secret keeps the stored one.
	post(s.saveBackupStorage, url.Values{"endpoint": {"https://x.r2.cloudflarestorage.com"}, "region": {"auto"}, "bucket": {"bkp2"}, "access_key": {"AK"}})
	if c := s.s3Config(); c.Bucket != "bkp2" || c.SecretKey != "SK" {
		t.Errorf("storage after update: %+v", c)
	}
}

func TestCreatePythonSite(t *testing.T) {
	s := newTestServer(t)
	form := url.Values{"repo": {"o/dj"}, "branch": {"main"}, "domain": {"dj.example.com"}, "node": {"24"}, "framework": {"django"}, "python": {"3.12"}}
	if w := postNewSite(s, form); w.Code != http.StatusSeeOther {
		t.Fatalf("django: %d %s", w.Code, w.Body.String())
	}
	sites, _ := s.st.Sites()
	var dj store.Site
	for _, x := range sites {
		if x.Domain == "dj.example.com" {
			dj = x
		}
	}
	if dj.Kind != store.KindPython || dj.PythonVersion != "3.12" || dj.NodeVersion != "" || int64(dj.Port) != store.AppPortBase+dj.ID ||
		!strings.Contains(dj.DeployScript, "uv pip install gunicorn") || !strings.Contains(dj.StartCommand, "gunicorn") {
		t.Errorf("django site = %+v", dj)
	}
	bad := url.Values{"repo": {"o/x"}, "branch": {"main"}, "domain": {"x.example.com"}, "framework": {"fastapi"}, "python": {"2.7"}}
	if w := postNewSite(s, bad); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("Python 2.7 accepted: %d", w.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.SetPathValue("id", "1")
	w := httptest.NewRecorder()
	s.sitePage(w, r, store.Session{Username: "admin"})
	if body := w.Body.String(); !strings.Contains(body, `name="python"`) || !strings.Contains(body, "ALLOWED_HOSTS") || strings.Contains(body, `name="php"`) {
		t.Error("Python site page should offer a Python picker and Django hints, not PHP")
	}
}
