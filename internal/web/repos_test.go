package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"skiff/internal/github"
	"skiff/internal/store"
)

// Organizations that restrict OAuth apps hide their repositories until an
// owner approves the app; the page says so and links to the approval screen.
func TestReposPageExplainsOrganizationAccess(t *testing.T) {
	s := newTestServer(t)
	for k, v := range map[string]string{keyGitHubToken: "tok", keyGitHubLogin: "octo", keyClientID: "Ov23liTEST"} {
		if err := s.st.SetSetting(k, v); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh cached list, so the page doesn't call GitHub.
	s.repoToken, s.repoAt = "tok", time.Now()
	s.repoList = []github.Repo{{FullName: "acme/api", DefaultBranch: "main"}}

	w := httptest.NewRecorder()
	s.reposPage(w, httptest.NewRequest(http.MethodGet, "/repos", nil), store.Session{})
	body := w.Body.String()
	for _, want := range []string{"acme/api", "Missing an organization", "https://github.com/settings/connections/applications/Ov23liTEST"} {
		if !strings.Contains(body, want) {
			t.Errorf("repos page is missing %q", want)
		}
	}
}
