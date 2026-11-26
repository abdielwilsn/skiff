package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"rsc.io/qr"

	"skiff/internal/store"
)

// Two-factor login with an authenticator app (TOTP, RFC 6238: SHA-1, 30-second
// steps, 6 digits), plus single-use recovery codes. "skiff disable-2fa" on the
// server turns it off if the phone is lost.

const (
	totpStep      = 30
	totpDigits    = 6
	pendingCookie = "skiff_2fa"
	pendingTTL    = 5 * time.Minute
	pendingTries  = 5
	recoveryCount = 10
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// totpCode is the code for a secret at a 30-second step.
func totpCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, n%1000000)
}

// totpMatch returns the step a code is valid for (now, or one step either
// side for clock drift), or 0.
func totpMatch(secretB32, code string, now time.Time) int64 {
	secret, err := b32.DecodeString(strings.ToUpper(secretB32))
	if err != nil || len(code) != totpDigits {
		return 0
	}
	cur := now.Unix() / totpStep
	for _, st := range []int64{cur, cur - 1, cur + 1} {
		if subtle.ConstantTimeCompare([]byte(totpCode(secret, st)), []byte(code)) == 1 {
			return st
		}
	}
	return 0
}

// twoFA keeps the in-memory state of logins and setups in progress.
type twoFA struct {
	mu      sync.Mutex
	pending map[string]*pendingLogin // cookie token -> login waiting for a code
	setup   map[int64]setupSecret    // user -> secret shown but not confirmed yet
	last    map[int64]int64          // user -> last step accepted (no replays)
}

type pendingLogin struct {
	userID  int64
	expires time.Time
	tries   int
}

type setupSecret struct {
	secret  string
	expires time.Time
}

func newTwoFA() *twoFA {
	return &twoFA{pending: map[string]*pendingLogin{}, setup: map[int64]setupSecret{}, last: map[int64]int64{}}
}

// checkCode accepts a current TOTP code (once) or an unused recovery code.
func (s *Server) checkCode(u store.User, code string) bool {
	code = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(code), " ", ""))
	if len(code) == totpDigits {
		st := totpMatch(u.TOTPSecret, code, time.Now())
		if st == 0 {
			return false
		}
		s.twofa.mu.Lock()
		defer s.twofa.mu.Unlock()
		if st <= s.twofa.last[u.ID] {
			return false // already used
		}
		s.twofa.last[u.ID] = st
		return true
	}
	if norm := strings.ReplaceAll(code, "-", ""); len(norm) == 10 {
		ok, err := s.st.UseRecoveryCode(u.ID, store.HashToken(norm))
		return err == nil && ok
	}
	return false
}

// newRecoveryCodes returns codes to show once, and their hashes to store.
func newRecoveryCodes() (codes, hashes []string) {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	for i := 0; i < recoveryCount; i++ {
		b := make([]byte, 10)
		_, _ = rand.Read(b)
		for j := range b {
			b[j] = alphabet[int(b[j])%len(alphabet)]
		}
		codes = append(codes, string(b[:5])+"-"+string(b[5:]))
		hashes = append(hashes, store.HashToken(string(b)))
	}
	return codes, hashes
}

// ---- login, second step ------------------------------------------------------

func (s *Server) startPendingLogin(w http.ResponseWriter, r *http.Request, userID int64) {
	tok := randomHex(32)
	s.twofa.mu.Lock()
	now := time.Now()
	for k, p := range s.twofa.pending {
		if now.After(p.expires) {
			delete(s.twofa.pending, k)
		}
	}
	s.twofa.pending[tok] = &pendingLogin{userID: userID, expires: now.Add(pendingTTL)}
	s.twofa.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: pendingCookie, Value: tok, Path: "/login", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.secureCookies(r), MaxAge: int(pendingTTL.Seconds())})
	redirect(w, r, "/login/2fa")
}

func (s *Server) pendingFrom(r *http.Request) (string, *pendingLogin) {
	c, err := r.Cookie(pendingCookie)
	if err != nil {
		return "", nil
	}
	s.twofa.mu.Lock()
	defer s.twofa.mu.Unlock()
	p := s.twofa.pending[c.Value]
	if p == nil || time.Now().After(p.expires) {
		delete(s.twofa.pending, c.Value)
		return "", nil
	}
	return c.Value, p
}

type login2FAData struct{ Error string }

func (s *Server) login2FAPage(w http.ResponseWriter, r *http.Request) {
	if _, p := s.pendingFrom(r); p == nil {
		redirect(w, r, "/login")
		return
	}
	s.render(w, r, "login_2fa", http.StatusOK, page{Title: "Two-factor login", Data: login2FAData{}})
}

func (s *Server) login2FASubmit(w http.ResponseWriter, r *http.Request) {
	tok, p := s.pendingFrom(r)
	if p == nil {
		setFlash(w, "err", "That took too long. Log in again.")
		redirect(w, r, "/login")
		return
	}
	ip := s.clientIP(r)
	if !s.limiter.allowed(ip) {
		s.render(w, r, "login_2fa", http.StatusTooManyRequests, page{Title: "Two-factor login",
			Data: login2FAData{Error: "Too many failed attempts. Try again in a few minutes."}})
		return
	}
	u, err := s.st.UserByID(p.userID)
	if err != nil || u.TOTPSecret == "" || !s.checkCode(u, r.FormValue("code")) {
		s.limiter.fail(ip)
		s.twofa.mu.Lock()
		p.tries++
		over := p.tries >= pendingTries
		if over {
			delete(s.twofa.pending, tok)
		}
		s.twofa.mu.Unlock()
		if over {
			setFlash(w, "err", "Too many wrong codes. Log in again.")
			redirect(w, r, "/login")
			return
		}
		s.render(w, r, "login_2fa", http.StatusUnauthorized, page{Title: "Two-factor login",
			Data: login2FAData{Error: "That code isn't right. Use the current code from your app, or a recovery code."}})
		return
	}
	s.twofa.mu.Lock()
	delete(s.twofa.pending, tok)
	s.twofa.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: pendingCookie, Path: "/login", MaxAge: -1, HttpOnly: true, Secure: s.secureCookies(r)})
	s.limiter.reset(ip)
	if err := s.startSession(w, r, u.ID); err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/")
}

// ---- settings ---------------------------------------------------------------

// securityData is the Security card on the Settings page.
type securityData struct {
	Enabled      bool
	RecoveryLeft int
	Sessions     int
	Setup        *totpSetupView // a secret being set up
	NewRecovery  []string       // shown once, right after turning it on
	Error        string
}

type totpSetupView struct {
	Secret string // grouped for typing
	URI    template.URL
	QR     template.URL // data: PNG
}

func (s *Server) securityFor(sess store.Session, d *securityData) {
	u, err := s.st.UserByID(sess.UserID)
	if err != nil {
		return
	}
	d.Enabled = u.TOTPSecret != ""
	d.RecoveryLeft = len(u.Recovery)
	d.Sessions, _ = s.st.SessionCount(u.ID)
	if d.Enabled {
		return
	}
	s.twofa.mu.Lock()
	st, ok := s.twofa.setup[u.ID]
	s.twofa.mu.Unlock()
	if !ok || time.Now().After(st.expires) {
		return
	}
	host := s.panelHTTPSDomain()
	if host == "" {
		host = "Skiff"
	}
	uri := "otpauth://totp/" + url.PathEscape("Skiff ("+host+"):"+u.Username) + "?" + url.Values{
		"secret": {st.secret}, "issuer": {"Skiff"}, "digits": {"6"}, "period": {"30"},
	}.Encode()
	v := &totpSetupView{URI: template.URL(uri)}
	for i := 0; i < len(st.secret); i += 4 {
		v.Secret += st.secret[i:min(i+4, len(st.secret))] + " "
	}
	v.Secret = strings.TrimSpace(v.Secret)
	if code, err := qr.Encode(uri, qr.M); err == nil {
		code.Scale = 6
		v.QR = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG()))
	}
	d.Setup = v
}

func (s *Server) start2FA(w http.ResponseWriter, r *http.Request, sess store.Session) {
	secret := make([]byte, 20)
	_, _ = rand.Read(secret)
	s.twofa.mu.Lock()
	s.twofa.setup[sess.UserID] = setupSecret{secret: b32.EncodeToString(secret), expires: time.Now().Add(15 * time.Minute)}
	s.twofa.mu.Unlock()
	redirect(w, r, "/settings#security")
}

func (s *Server) enable2FA(w http.ResponseWriter, r *http.Request, sess store.Session) {
	s.twofa.mu.Lock()
	st, ok := s.twofa.setup[sess.UserID]
	s.twofa.mu.Unlock()
	if !ok || time.Now().After(st.expires) {
		setFlash(w, "err", "The setup expired. Start again.")
		redirect(w, r, "/settings#security")
		return
	}
	step := totpMatch(st.secret, strings.ReplaceAll(strings.TrimSpace(r.FormValue("code")), " ", ""), time.Now())
	if step == 0 {
		setFlash(w, "err", "That code doesn't match. Check the time on your phone, and enter the current code.")
		redirect(w, r, "/settings#security")
		return
	}
	codes, hashes := newRecoveryCodes()
	if err := s.st.SetTOTP(sess.UserID, st.secret, hashes); err != nil {
		s.serverError(w, err)
		return
	}
	s.twofa.mu.Lock()
	delete(s.twofa.setup, sess.UserID)
	s.twofa.last[sess.UserID] = step
	s.twofa.mu.Unlock()
	// Anyone already logged in elsewhere logged in without the second factor.
	if c, err := r.Cookie(sessionCookie); err == nil {
		_, _ = s.st.DeleteOtherSessions(sess.UserID, c.Value)
	}
	// The codes are shown once, in this response, and never stored in plain text.
	s.renderSettings(w, r, sess, func(d *settingsData) {
		d.Security.NewRecovery = codes
		d.Flash = "Two-factor login is on. Other sessions were logged out."
	})
}

func (s *Server) disable2FA(w http.ResponseWriter, r *http.Request, sess store.Session) {
	u, err := s.st.UserByID(sess.UserID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(r.FormValue("password"))) != nil || !s.checkCode(u, r.FormValue("code")) {
		setFlash(w, "err", "To turn two-factor login off, enter your password and a current code (or a recovery code).")
		redirect(w, r, "/settings#security")
		return
	}
	if err := s.st.SetTOTP(u.ID, "", nil); err != nil {
		s.serverError(w, err)
		return
	}
	setFlash(w, "ok", "Two-factor login is off.")
	redirect(w, r, "/settings#security")
}

func (s *Server) revokeSessions(w http.ResponseWriter, r *http.Request, sess store.Session) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		redirect(w, r, "/login")
		return
	}
	n, err := s.st.DeleteOtherSessions(sess.UserID, c.Value)
	if err != nil {
		s.serverError(w, err)
		return
	}
	setFlash(w, "ok", fmt.Sprintf("Logged out %d other session(s).", n))
	redirect(w, r, "/settings#security")
}
