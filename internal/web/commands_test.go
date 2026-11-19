package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"skiff/internal/store"
)

func postCommand(s *Server, siteID, command string) *httptest.ResponseRecorder {
	form := url.Values{"command": {command}}
	r := httptest.NewRequest(http.MethodPost, "/sites/"+siteID+"/commands", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetPathValue("id", siteID)
	w := httptest.NewRecorder()
	s.runCommand(w, r, store.Session{Username: "admin"})
	return w
}

func TestRunCommandHandler(t *testing.T) {
	s := newTestServer(t)
	id, _ := s.st.CreateSite(store.Site{Domain: "app.example.com", Repo: "o/r", Branch: "main", PHPVersion: "8.4", NodeVersion: "24", DeployScript: "true\n"})

	if w := postCommand(s, "1", "php artisan migrate --force"); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "Deploy the site once") {
		t.Fatalf("undeployed site: %d", w.Code)
	}
	_ = s.st.MarkSiteDeployed(id)
	if w := postCommand(s, "1", "   "); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty command: %d", w.Code)
	}

	cmd := "php artisan migrate --force\r\nphp artisan db:seed --force"
	w := postCommand(s, "1", cmd)
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/jobs/") {
		t.Fatalf("run: %d %s", w.Code, w.Header().Get("Location"))
	}
	hist, _ := s.st.SiteJobsOfKind(id, "command", 10)
	if len(hist) != 1 || hist[0].Detail != "php artisan migrate --force\nphp artisan db:seed --force" ||
		hist[0].Title != "Run php artisan migrate --force …" {
		t.Fatalf("history = %+v", hist)
	}
	// The dry-run executor completes the job; wait so the runner isn't mid-write at cleanup.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ := s.st.Job(hist[0].ID); j.Done() {
			if j.Status != store.JobSuccess {
				t.Fatalf("command job status %s", j.Status)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("command job did not finish")
}

func TestCommandTitle(t *testing.T) {
	if got := commandTitle(strings.Repeat("x", 100)); len([]rune(got)) != 70 {
		t.Errorf("long title not truncated: %d runes", len([]rune(got)))
	}
	if got := commandTitle("php artisan about"); got != "php artisan about" {
		t.Errorf("got %q", got)
	}
}
