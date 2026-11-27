package web

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"skiff/internal/store"
)

const (
	sessionCookie = "skiff_session"
	sessionTTL    = 30 * 24 * time.Hour
)

type handler func(w http.ResponseWriter, r *http.Request, sess store.Session)

// auth requires a logged-in admin and, for POSTs, a valid CSRF token.
func (s *Server) auth(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.needsSetup() {
			redirect(w, r, "/setup")
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			redirect(w, r, "/login")
			return
		}
		sess, err := s.st.Session(c.Value)
		if err != nil {
			s.clearSessionCookie(w, r)
			redirect(w, r, "/login")
			return
		}
		if r.Method == http.MethodPost {
			// A header for requests whose body isn't a form (file uploads):
			// reading the form would read, and buffer, the whole body.
			token := r.Header.Get("X-CSRF-Token")
			if token == "" {
				token = r.FormValue("csrf")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(sess.CSRF)) != 1 {
				http.Error(w, "Invalid or missing CSRF token. Reload the page and try again.", http.StatusForbidden)
				return
			}
		}
		h(w, r, sess)
	}
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	token, err := s.st.CreateSession(userID, sessionTTL)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies(r),
		MaxAge: int(sessionTTL.Seconds()),
	})
	return nil
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secureCookies(r)})
}

// ---- setup -----------------------------------------------------------------

type setupData struct {
	Error, Token, Username, TokenPath string
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if !s.needsSetup() {
		redirect(w, r, "/login")
		return
	}
	s.render(w, r, "setup", http.StatusOK, page{Title: "Set up", Data: setupData{
		Token: r.URL.Query().Get("token"), TokenPath: s.setupTokenPath(),
	}})
}

func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.needsSetup() {
		redirect(w, r, "/login")
		return
	}
	d := setupData{
		Token:     strings.TrimSpace(r.FormValue("token")),
		Username:  strings.TrimSpace(r.FormValue("username")),
		TokenPath: s.setupTokenPath(),
	}
	password, confirm := r.FormValue("password"), r.FormValue("confirm")

	s.setupMu.Lock()
	expected := s.setupToken
	s.setupMu.Unlock()

	switch {
	case subtle.ConstantTimeCompare([]byte(d.Token), []byte(expected)) != 1:
		d.Error = "That setup token is not correct."
	case !usernameRe.MatchString(d.Username):
		d.Error = "Username must be 3–32 letters, numbers, dots, dashes or underscores."
	case len(password) < 10:
		d.Error = "Password must be at least 10 characters."
	case password != confirm:
		d.Error = "Passwords do not match."
	}
	if d.Error != "" {
		s.render(w, r, "setup", http.StatusUnprocessableEntity, page{Title: "Set up", Data: d})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id, err := s.st.CreateUser(d.Username, string(hash))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.setupMu.Lock()
	s.setupToken = ""
	s.setupMu.Unlock()
	_ = os.Remove(s.setupTokenPath())

	if err := s.startSession(w, r, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setFlash(w, "ok", "Welcome aboard. Start by provisioning this server.")
	redirect(w, r, "/")
}

// ---- login -----------------------------------------------------------------

type loginData struct{ Error, Username string }

// dummyHash keeps login timing the same whether or not the username exists.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("skiff-timing-equaliser"), bcrypt.DefaultCost)

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.needsSetup() {
		redirect(w, r, "/setup")
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if _, err := s.st.Session(c.Value); err == nil {
			redirect(w, r, "/")
			return
		}
	}
	s.render(w, r, "login", http.StatusOK, page{Title: "Log in", Data: loginData{}})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if s.needsSetup() {
		redirect(w, r, "/setup")
		return
	}
	ip := s.clientIP(r)
	d := loginData{Username: strings.TrimSpace(r.FormValue("username"))}
	if !s.limiter.allowed(ip) {
		d.Error = "Too many failed attempts. Try again in a few minutes."
		s.render(w, r, "login", http.StatusTooManyRequests, page{Title: "Log in", Data: d})
		return
	}
	u, err := s.st.UserByName(d.Username)
	hash := []byte(u.PasswordHash)
	if err != nil {
		hash = dummyHash
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(r.FormValue("password"))) != nil || err != nil {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.limiter.fail(ip)
		d.Error = "Wrong username or password."
		s.render(w, r, "login", http.StatusUnauthorized, page{Title: "Log in", Data: d})
		return
	}
	// The password was right; with two-factor login on, a code comes next.
	// The failure count isn't reset until that step succeeds.
	if u.TOTPSecret != "" {
		s.startPendingLogin(w, r, u.ID)
		return
	}
	s.limiter.reset(ip)
	if err := s.startSession(w, r, u.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirect(w, r, "/")
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, _ store.Session) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.st.DeleteSession(c.Value)
	}
	s.clearSessionCookie(w, r)
	redirect(w, r, "/login")
}

// proxyHeader carries a secret only Skiff's Nginx proxy knows (its config is
// root-only), proving a local request came through that proxy.
const proxyHeader = "X-Skiff-Proxy"

// clientIP is the address to rate-limit by. Behind Skiff's own Nginx proxy
// every request comes from loopback, so the X-Real-IP that proxy sets (it
// overwrites any client-supplied value) is trusted, but only when the request
// also carries the proxy's secret. Any other local process (every site runs on
// this machine) is keyed by its own address, so it can't invent a new client
// IP per attempt and get around the login limit.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	if isLoopback(r) && s.fromProxy(r) {
		if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip != nil {
			return ip.String()
		}
	}
	return host
}

func (s *Server) fromProxy(r *http.Request) bool {
	got := r.Header.Get(proxyHeader)
	return s.proxySecret != "" && got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.proxySecret)) == 1
}

// limiter allows 10 failed logins per IP per 15 minutes.
type limiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

const (
	limitWindow = 15 * time.Minute
	limitMax    = 10
)

func newLimiter() *limiter { return &limiter{fails: map[string][]time.Time{}} }

func (l *limiter) prune(ip string) []time.Time {
	cut := time.Now().Add(-limitWindow)
	kept := l.fails[ip][:0]
	for _, t := range l.fails[ip] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, ip)
		return nil
	}
	l.fails[ip] = kept
	return kept
}

func (l *limiter) allowed(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(ip)) < limitMax
}

func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[ip] = append(l.prune(ip), time.Now())
}

func (l *limiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}
