package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"skiff/internal/executor"
	"skiff/internal/jobs"
	"skiff/internal/store"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "skiff.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, err := New(Config{
		Store: st, Runner: jobs.NewRunner(ctx, st, dir), Exec: executor.DryRun{},
		DataDir: dir, ListenAddr: ":8090", Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func get(s *Server, remote, path string, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = remote
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestPanelDomainRedirect(t *testing.T) {
	s := newTestServer(t)

	// No panel domain: served directly.
	if w := get(s, "203.0.113.5:4000", "/login?x=1", nil); w.Code == http.StatusTemporaryRedirect {
		t.Fatalf("redirected without a panel domain: %s", w.Header().Get("Location"))
	}

	if err := s.setPanelHTTPSDomain("panel.example.com"); err != nil {
		t.Fatal(err)
	}
	w := get(s, "203.0.113.5:4000", "/login?x=1", nil)
	if w.Code != http.StatusTemporaryRedirect || w.Header().Get("Location") != "https://panel.example.com/login?x=1" {
		t.Fatalf("external request: got %d %q", w.Code, w.Header().Get("Location"))
	}
	// Through the Nginx proxy or an SSH tunnel: served, not redirected.
	for _, remote := range []string{"127.0.0.1:5000", "[::1]:5000"} {
		if w := get(s, remote, "/login", nil); w.Code == http.StatusTemporaryRedirect {
			t.Fatalf("loopback %s was redirected", remote)
		}
	}
	if got := s.baseURL(httptest.NewRequest(http.MethodGet, "/", nil)); got != "https://panel.example.com" {
		t.Fatalf("baseURL = %q", got)
	}

	// Removing the domain stops the redirect immediately.
	if err := s.setPanelHTTPSDomain(""); err != nil {
		t.Fatal(err)
	}
	if w := get(s, "203.0.113.5:4000", "/login", nil); w.Code == http.StatusTemporaryRedirect {
		t.Fatal("still redirecting after the domain was removed")
	}
}

func TestClientIPTrustsOnlyLocalProxy(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Real-IP", "198.51.100.7")

	r.RemoteAddr = "127.0.0.1:1234"
	if got := clientIP(r); got != "198.51.100.7" {
		t.Errorf("behind local proxy: clientIP = %q", got)
	}
	r.RemoteAddr = "203.0.113.5:1234"
	if got := clientIP(r); got != "203.0.113.5" {
		t.Errorf("spoofed header from outside was trusted: clientIP = %q", got)
	}
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Real-IP", "not-an-ip")
	if got := clientIP(r); got != "127.0.0.1" {
		t.Errorf("garbage header: clientIP = %q", got)
	}
}
