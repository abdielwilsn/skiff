package web

import (
	"context"
	"crypto/tls"
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
	if header["tls"] == "1" {
		r.TLS = &tls.ConnectionState{}
		delete(header, "tls")
	}
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestPanelDomainRedirect(t *testing.T) {
	s := newTestServer(t)

	// No panel domain: served over the port's own HTTPS. Plain HTTP from
	// outside is sent there, so passwords never cross the network in clear.
	if w := get(s, "203.0.113.5:4000", "/login?x=1", map[string]string{"tls": "1"}); w.Code == http.StatusTemporaryRedirect {
		t.Fatalf("HTTPS request redirected without a panel domain: %s", w.Header().Get("Location"))
	}
	if w := get(s, "203.0.113.5:4000", "/login?x=1", nil); w.Code != http.StatusTemporaryRedirect || w.Header().Get("Location") != "https://example.com/login?x=1" {
		t.Fatalf("plain HTTP from outside: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := get(s, "127.0.0.1:4000", "/login", nil); w.Code == http.StatusTemporaryRedirect {
		t.Fatal("plain HTTP from this machine (SSH tunnel) was redirected")
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
	if w := get(s, "203.0.113.5:4000", "/login", map[string]string{"tls": "1"}); w.Code == http.StatusTemporaryRedirect {
		t.Fatal("still redirecting after the domain was removed")
	}
}

func TestClientIPTrustsOnlyLocalProxy(t *testing.T) {
	s := newTestServer(t)
	if len(s.proxySecret) < 32 {
		t.Fatalf("proxy secret %q", s.proxySecret)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Real-IP", "198.51.100.7")
	r.Header.Set(proxyHeader, s.proxySecret)

	r.RemoteAddr = "127.0.0.1:1234"
	if got := s.clientIP(r); got != "198.51.100.7" {
		t.Errorf("behind local proxy: clientIP = %q", got)
	}
	r.RemoteAddr = "203.0.113.5:1234"
	if got := s.clientIP(r); got != "203.0.113.5" {
		t.Errorf("spoofed header from outside was trusted: clientIP = %q", got)
	}
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Real-IP", "not-an-ip")
	if got := s.clientIP(r); got != "127.0.0.1" {
		t.Errorf("garbage header: clientIP = %q", got)
	}
	// Another local process (a site) without the proxy's secret can't pick
	// its own client IP, so it can't dodge the login limit.
	r.Header.Set("X-Real-IP", "198.51.100.8")
	for _, secret := range []string{"", "wrong"} {
		r.Header.Set(proxyHeader, secret)
		if got := s.clientIP(r); got != "127.0.0.1" {
			t.Errorf("secret %q: clientIP = %q", secret, got)
		}
	}
}
