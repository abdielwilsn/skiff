package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"skiff/internal/store"
)

func signBody(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func deliver(s *Server, siteID, event, body, sig string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/hooks/github/"+siteID, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:5000" // via the panel's Nginx proxy
	r.Header.Set("X-GitHub-Event", event)
	if sig != "" {
		r.Header.Set("X-Hub-Signature-256", sig)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestGitHubWebhook(t *testing.T) {
	s := newTestServer(t)
	id, err := s.st.CreateSite(store.Site{Domain: "shop.example.com", Repo: "acme/shop", Branch: "main",
		PHPVersion: "8.4", NodeVersion: "24", DeployScript: "true\n"})
	if err != nil {
		t.Fatal(err)
	}
	sid := "1"
	push := `{"ref":"refs/heads/main","after":"a1b2c3d4e5f6","pusher":{"name":"alice"}}`

	// Auto-deploy off: the endpoint doesn't exist for this site.
	if w := deliver(s, sid, "push", push, signBody("x", push)); w.Code != http.StatusNotFound {
		t.Fatalf("auto-deploy off: got %d", w.Code)
	}

	secret := strings.Repeat("w", 32) // built at runtime so secret scanners ignore it
	if err := s.st.SetAutoDeploy(id, true, secret, 77); err != nil {
		t.Fatal(err)
	}

	for name, sig := range map[string]string{"unsigned": "", "wrong secret": signBody("nope", push)} {
		if w := deliver(s, sid, "push", push, sig); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", name, w.Code)
		}
	}
	if w := deliver(s, sid, "ping", `{}`, signBody(secret, `{}`)); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "pong") {
		t.Errorf("ping: %d %q", w.Code, w.Body.String())
	}
	other := `{"ref":"refs/heads/feature-x","after":"ffff"}`
	if w := deliver(s, sid, "push", other, signBody(secret, other)); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ignored") {
		t.Errorf("other branch: %d %q", w.Code, w.Body.String())
	}
	if jobs, _ := s.st.SiteJobs(id, 10); len(jobs) != 0 {
		t.Fatalf("ignored deliveries created %d jobs", len(jobs))
	}

	w := deliver(s, sid, "push", push, signBody(secret, push))
	if w.Code != http.StatusAccepted {
		t.Fatalf("push to main: %d %q", w.Code, w.Body.String())
	}
	jobs, _ := s.st.SiteJobs(id, 10)
	if len(jobs) != 1 || jobs[0].Kind != "deploy" || jobs[0].Title != "Deploy shop.example.com (push a1b2c3d by alice)" {
		t.Fatalf("jobs after push = %+v", jobs)
	}

	// Let the (dry-run) deploy finish so the test doesn't race the runner.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ := s.st.Job(jobs[0].ID); j.Done() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestWebhookIsPublicButOtherRoutesAreNot(t *testing.T) {
	s := newTestServer(t)
	// No admin exists yet: normal pages redirect to setup, the hook endpoint answers itself.
	if w := deliver(s, "1", "push", `{}`, ""); w.Code == http.StatusSeeOther {
		t.Fatal("webhook endpoint should not require a login")
	}
	r := httptest.NewRequest(http.MethodPost, "/sites/1/autodeploy", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("enabling auto-deploy must require a login, got %d", w.Code)
	}
}
