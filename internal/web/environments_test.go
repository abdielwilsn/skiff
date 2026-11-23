package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"skiff/internal/store"
)

const prodEnv = "APP_NAME=App\nAPP_ENV=production\nAPP_URL=https://app.example.com\nDB_CONNECTION=mysql\nDB_DATABASE=app_example_com\nSESSION_DOMAIN=.app.example.com\n"

func sitePost(s *Server, h func(http.ResponseWriter, *http.Request, store.Session), id int64, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetPathValue("id", strconv.FormatInt(id, 10))
	w := httptest.NewRecorder()
	h(w, r, store.Session{Username: "admin"})
	return w
}

func siteGet(t *testing.T, s *Server, h func(http.ResponseWriter, *http.Request, store.Session), id int64) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.SetPathValue("id", strconv.FormatInt(id, 10))
	w := httptest.NewRecorder()
	h(w, r, store.Session{Username: "admin"})
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

// waitJobs waits for every queued job to finish.
func waitJobs(t *testing.T, s *Server) []store.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		jobs, _ := s.st.RecentJobs(50)
		done := true
		for _, j := range jobs {
			done = done && j.Done()
		}
		if done {
			return jobs
		}
		if time.Now().After(deadline) {
			t.Fatalf("jobs still running: %+v", jobs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCreateStagingEnvironment(t *testing.T) {
	s, f := newFakeServer(t)
	_ = s.st.SetSetting(keyMySQLInstalled, "1")
	prod := laravelSiteWithDatabases(t, s)
	_ = s.st.SetWorkers(prod.ID, 2, "--tries=3", true)
	f.respond = func(script string, w io.Writer) bool {
		if strings.Contains(script, `cat -- "$ENVF"`) && strings.Contains(script, "/home/skiff/app.example.com'") {
			io.WriteString(w, prodEnv)
			return true
		}
		return false
	}

	// Invalid names, taken domains and "production" are refused.
	for _, form := range []url.Values{
		{"name": {"production"}, "domain": {"staging.app.example.com"}, "branch": {"staging"}},
		{"name": {"Staging!"}, "domain": {"staging.app.example.com"}, "branch": {"staging"}},
		{"name": {"staging"}, "domain": {"app.example.com"}, "branch": {"staging"}},
	} {
		if w := sitePost(s, s.createEnvironment, prod.ID, form); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%v accepted: %d", form, w.Code)
		}
	}

	w := sitePost(s, s.createEnvironment, prod.ID, url.Values{"name": {"staging"}, "domain": {"staging.app.example.com"}, "branch": {"develop"}, "copy_env": {"1"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	jobs := waitJobs(t, s)
	envs, _ := s.st.Environments(prod)
	if len(envs) != 2 || envs[0].ID != prod.ID {
		t.Fatalf("environments = %+v", envs)
	}
	st := envs[1]
	if st.Environment != "staging" || st.ParentID != prod.ID || st.Branch != "develop" || st.Repo != prod.Repo ||
		st.PHPVersion != prod.PHPVersion || st.DeployScript != prod.DeployScript || st.WorkerCount != 2 || !st.Scheduler {
		t.Errorf("staging = %+v", st)
	}
	db, err := s.st.DatabaseForSite(st.ID)
	if err != nil || db.Name == "app_example_com" {
		t.Errorf("staging should get its own database, got %+v %v", db, err)
	}
	for _, j := range jobs {
		if j.Status != store.JobSuccess {
			t.Errorf("job %q: %s", j.Title, j.Status)
		}
	}

	// The .env was copied with APP_ENV and the site's URLs changed, before
	// the deploy pointed it at the new database.
	var seeded string
	for _, in := range f.stdins {
		if strings.Contains(in, "APP_NAME=App") {
			seeded = in
		}
	}
	if !strings.Contains(seeded, "APP_ENV=staging") || !strings.Contains(seeded, "APP_URL=http://staging.app.example.com") ||
		!strings.Contains(seeded, "SESSION_DOMAIN=.staging.app.example.com") {
		t.Errorf("seeded .env:\n%s", seeded)
	}
	seedAt, dbAt := -1, -1
	for i, sc := range f.scripts {
		if strings.Contains(sc, "/home/skiff/staging.app.example.com'/shared") && seedAt < 0 {
			seedAt = i
		}
		if strings.Contains(sc, "set_env DB_DATABASE "+db.Name) {
			dbAt = i
		}
	}
	if seedAt < 0 || dbAt < seedAt {
		t.Errorf("seed at %d, database env at %d", seedAt, dbAt)
	}

	// Production can't be deleted while it has environments.
	sitePost(s, s.deleteSite, prod.ID, url.Values{"confirm": {prod.Domain}})
	if _, err := s.st.Site(prod.ID); err != nil {
		t.Error("production was deleted with a staging environment")
	}

	// Pages: one card on the dashboard with a staging chip, a switcher in the sidebar.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rw := httptest.NewRecorder()
	s.dashboard(rw, r, store.Session{Username: "admin"})
	if body := rw.Body.String(); strings.Count(body, `class="card site-card"`) != 1 || !strings.Contains(body, `class="env-chip"`) {
		t.Error("dashboard should group staging under production")
	}
	body := siteGet(t, s, s.sitePage, st.ID)
	if !strings.Contains(body, "staging</span>") || !strings.Contains(body, "environment of <a href=\"/sites/1\">app.example.com</a>") {
		t.Error("staging overview should say which site it belongs to")
	}
	if body := siteGet(t, s, s.environmentsPage, prod.ID); !strings.Contains(body, "staging.app.example.com") || !strings.Contains(body, `value="preview"`) {
		t.Error("environments page should list staging and suggest preview next")
	}
}

func TestPromoteDeploysStagingCommit(t *testing.T) {
	s, f := newFakeServer(t)
	prod := laravelSiteWithDatabases(t, s)
	id, err := s.st.CreateSite(store.Site{Domain: "staging.app.example.com", Repo: prod.Repo, Branch: "develop", PHPVersion: "8.4", DeployScript: "true", Environment: "staging", ParentID: prod.ID})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.st.MarkSiteDeployed(id)
	sha := strings.Repeat("1f", 20)
	f.respond = func(script string, w io.Writer) bool {
		if strings.Contains(script, ".skiff-commit\"") && strings.Contains(script, "staging.app.example.com") {
			io.WriteString(w, "20261123120000\t1\t"+sha+"\t1f1f1f1 feat: new checkout\n")
			return true
		}
		return false
	}

	// A stale page (staging was deployed again since) promotes nothing.
	sitePost(s, s.promote, id, url.Values{"commit": {strings.Repeat("2e", 20)}})
	// Production has nothing to promote to.
	sitePost(s, s.promote, prod.ID, nil)
	if jobs, _ := s.st.RecentJobs(10); len(jobs) != 0 {
		t.Fatalf("queued %+v", jobs)
	}

	w := sitePost(s, s.promote, id, url.Values{"commit": {sha}})
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/jobs/") {
		t.Fatalf("promote: %d %s", w.Code, w.Header().Get("Location"))
	}
	jobs := waitJobs(t, s)
	if len(jobs) != 1 || *jobs[0].SiteID != prod.ID || !strings.Contains(jobs[0].Title, "promote 1f1f1f1 from staging") || jobs[0].Status != store.JobSuccess {
		t.Fatalf("jobs = %+v", jobs)
	}
	if f.ran("REF='"+sha+"'") != 1 || f.ran("SITE='/home/skiff/app.example.com'\nREPO=") != 1 {
		t.Error("production should be built from staging's commit")
	}
}
