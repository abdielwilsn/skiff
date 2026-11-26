package web

import (
	"crypto/tls"
	"encoding/base32"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// One port answers HTTPS (self-signed) and plain HTTP.
func TestMuxListenerServesTLSAndPlain(t *testing.T) {
	dir := t.TempDir()
	cfg, fp, err := PanelTLS(dir)
	if err != nil || len(fp) != 95 {
		t.Fatalf("cert: %v %q", err, fp)
	}
	if again, _ := TLSFingerprint(dir); again != fp {
		t.Fatal("certificate not reused")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			io.WriteString(w, "tls")
		} else {
			io.WriteString(w, "plain")
		}
	})}
	go srv.Serve(NewMuxListener(ln, cfg))
	defer srv.Close()
	addr := ln.Addr().String()

	// A client that connects and says nothing doesn't block others.
	idle, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()

	res, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != "plain" {
		t.Errorf("plain: %q", b)
	}
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	res, err = c.Get("https://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != "tls" || Fingerprint(res.TLS.PeerCertificates[0].Raw) != fp {
		t.Errorf("tls: %q", b)
	}
}

func TestTOTP(t *testing.T) {
	// RFC 6238 test vector (SHA-1, T = 59s): 94287082, last 6 digits.
	secret := []byte("12345678901234567890")
	if got := totpCode(secret, 59/30); got != "287082" {
		t.Fatalf("code = %s", got)
	}
	b := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
	now := time.Unix(59, 0)
	if totpMatch(b, "287082", now) == 0 || totpMatch(b, "287082", now.Add(30*time.Second)) == 0 {
		t.Error("current or previous step should match")
	}
	if totpMatch(b, "287082", now.Add(90*time.Second)) != 0 || totpMatch(b, "000000", now) != 0 {
		t.Error("stale or wrong code matched")
	}
}

func postForm(s *Server, path string, form url.Values, cookies []*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "127.0.0.1:1000"
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestTwoFactorLogin(t *testing.T) {
	s := newTestServer(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	id, _ := s.st.CreateUser("admin", string(hash))
	s.setupMu.Lock()
	s.setupToken = ""
	s.setupMu.Unlock()
	secret := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	codes, hashes := newRecoveryCodes()
	if err := s.st.SetTOTP(id, secret, hashes); err != nil {
		t.Fatal(err)
	}

	// The right password alone doesn't log in: it asks for a code.
	w := postForm(s, "/login", url.Values{"username": {"admin"}, "password": {"correct horse"}}, nil)
	if w.Header().Get("Location") != "/login/2fa" || hasCookie(w, sessionCookie) {
		t.Fatalf("password step: %d %q", w.Code, w.Header().Get("Location"))
	}
	pending := w.Result().Cookies()

	// A wrong code is refused; the right one logs in, once.
	if w := postForm(s, "/login/2fa", url.Values{"code": {"000000"}}, pending); w.Code != http.StatusUnauthorized || hasCookie(w, sessionCookie) {
		t.Fatalf("wrong code: %d", w.Code)
	}
	raw, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	code := totpCode(raw, time.Now().Unix()/30)
	w = postForm(s, "/login/2fa", url.Values{"code": {code}}, pending)
	if !hasCookie(w, sessionCookie) {
		t.Fatalf("right code: %d %s", w.Code, w.Body.String())
	}
	w = postForm(s, "/login", url.Values{"username": {"admin"}, "password": {"correct horse"}}, nil)
	if w := postForm(s, "/login/2fa", url.Values{"code": {code}}, w.Result().Cookies()); hasCookie(w, sessionCookie) {
		t.Fatal("a used code worked twice")
	}

	// A recovery code works once.
	w = postForm(s, "/login", url.Values{"username": {"admin"}, "password": {"correct horse"}}, nil)
	pending = w.Result().Cookies()
	if w := postForm(s, "/login/2fa", url.Values{"code": {strings.ToUpper(codes[0])}}, pending); !hasCookie(w, sessionCookie) {
		t.Fatal("recovery code refused")
	}
	if u, _ := s.st.UserByID(id); len(u.Recovery) != recoveryCount-1 {
		t.Errorf("recovery codes left: %d", len(u.Recovery))
	}

	// No pending login: straight back to the password.
	if w := postForm(s, "/login/2fa", url.Values{"code": {code}}, nil); w.Header().Get("Location") != "/login" {
		t.Fatal("2fa step without a password step")
	}
	if n, _ := s.st.DisableAllTOTP(); n != 1 {
		t.Errorf("disable-2fa turned off %d", n)
	}
}

func hasCookie(w *httptest.ResponseRecorder, name string) bool {
	for _, c := range w.Result().Cookies() {
		if c.Name == name && c.MaxAge >= 0 && c.Value != "" {
			return true
		}
	}
	return false
}

func TestTwoFactorSetupAndSessions(t *testing.T) {
	s := newTestServer(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	id, _ := s.st.CreateUser("admin", string(hash))
	tok, _ := s.st.CreateSession(id, time.Hour)
	other, _ := s.st.CreateSession(id, time.Hour)
	sess, _ := s.st.Session(tok)
	me := []*http.Cookie{{Name: sessionCookie, Value: tok}}
	call := func(h handler, form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for _, c := range me {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h(w, r, sess)
		return w
	}
	call(s.start2FA, nil)
	var d settingsData
	s.securityFor(sess, &d.Security)
	if d.Security.Setup == nil || !strings.HasPrefix(string(d.Security.Setup.QR), "data:image/png;base64,") || !strings.Contains(string(d.Security.Setup.URI), "otpauth://totp/") {
		t.Fatalf("setup view: %+v", d.Security.Setup)
	}
	secret := strings.ReplaceAll(d.Security.Setup.Secret, " ", "")
	if call(s.enable2FA, url.Values{"code": {"000000"}}); true {
		if u, _ := s.st.UserByID(id); u.TOTPSecret != "" {
			t.Fatal("enabled with a wrong code")
		}
	}
	raw, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	w := call(s.enable2FA, url.Values{"code": {totpCode(raw, time.Now().Unix()/30)}})
	body := w.Body.String()
	if u, _ := s.st.UserByID(id); u.TOTPSecret != secret || len(u.Recovery) != recoveryCount {
		t.Fatalf("not enabled: %+v", u)
	}
	if strings.Count(body, `<li><code>`) != recoveryCount || !strings.Contains(body, "Two-factor login is on") {
		t.Error("recovery codes should be shown once")
	}
	if _, err := s.st.Session(other); err == nil {
		t.Error("other sessions should end when two-factor login is turned on")
	}
	if _, err := s.st.Session(tok); err != nil {
		t.Error("this session should stay")
	}
	// Log out other sessions.
	another, _ := s.st.CreateSession(id, time.Hour)
	call(s.revokeSessions, nil)
	if _, err := s.st.Session(another); err == nil {
		t.Error("revoke left another session")
	}
	// Turning it off needs the password and a code.
	call(s.disable2FA, url.Values{"password": {"wrong"}, "code": {"000000"}})
	if u, _ := s.st.UserByID(id); u.TOTPSecret == "" {
		t.Fatal("turned off without the password")
	}
}
