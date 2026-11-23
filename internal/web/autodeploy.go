package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"skiff/internal/github"
	"skiff/internal/store"
)

// Push to deploy: Skiff adds a GitHub webhook to the site's repository that
// POSTs push events to /hooks/github/{site id}, signed with a per-site secret.
// A push to the site's branch queues a deploy. Every deploy fetches the newest
// commit, so pushes that arrive while one is queued are folded into it.

// publicHTTPSBase is the panel's public HTTPS origin, which GitHub can reach,
// or "" if the panel isn't on one yet.
func (s *Server) publicHTTPSBase() string {
	if strings.HasPrefix(s.cfg.BaseURL, "https://") {
		return strings.TrimRight(s.cfg.BaseURL, "/")
	}
	if domain := s.panelHTTPSDomain(); domain != "" {
		return "https://" + domain
	}
	return ""
}

func hookPath(siteID int64) string { return "/hooks/github/" + strconv.FormatInt(siteID, 10) }

func (s *Server) enableAutoDeploy(w http.ResponseWriter, r *http.Request, _ store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	fail := func(msg string) {
		setFlash(w, "err", msg)
		redirect(w, r, s.siteURL(site)+"/settings#autodeploy")
	}
	if site.AutoDeploy {
		redirect(w, r, s.siteURL(site)+"/settings#autodeploy")
		return
	}
	token := s.githubToken()
	if token == "" {
		fail("Connect GitHub in Settings first; Skiff adds the webhook through your GitHub account.")
		return
	}
	base := s.publicHTTPSBase()
	if base == "" {
		fail("GitHub has to reach this panel over HTTPS. Set up Settings → Panel address & HTTPS first.")
		return
	}

	secret := randomHex(32)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	hookID, err := github.CreatePushHook(ctx, token, site.Repo, base+hookPath(site.ID), secret)
	switch {
	case errors.Is(err, github.ErrNotFound):
		login, _ := s.st.Setting(keyGitHubLogin)
		fail(fmt.Sprintf("GitHub refused: @%s needs admin access to %s to add a webhook.", login, site.Repo))
		return
	case errors.Is(err, github.ErrUnauthorized):
		fail("GitHub access was revoked. Reconnect GitHub in Settings and try again.")
		return
	case err != nil && strings.Contains(err.Error(), "already exists"):
		fail("This repo already has a webhook for this site's URL. Delete it in the repo's Settings → Webhooks on GitHub, then try again.")
		return
	case err != nil:
		fail("Couldn't add the GitHub webhook: " + err.Error())
		return
	}
	if err := s.st.SetAutoDeploy(site.ID, true, secret, hookID); err != nil {
		s.serverError(w, err)
		return
	}
	setFlash(w, "ok", "Auto-deploy is on. Every push to "+site.Branch+" deploys "+site.Domain+".")
	redirect(w, r, s.siteURL(site)+"/settings#autodeploy")
}

func (s *Server) disableAutoDeploy(w http.ResponseWriter, r *http.Request, _ store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	msg := "Auto-deploy is off. Use Deploy now to ship new commits."
	if err := s.removeWebhook(r.Context(), site); err != nil {
		msg = "Auto-deploy is off, but the webhook couldn't be removed from GitHub (" + err.Error() +
			"). Delete it in the repo's Settings → Webhooks; until then GitHub's deliveries are ignored."
	}
	if err := s.st.SetAutoDeploy(site.ID, false, "", 0); err != nil {
		s.serverError(w, err)
		return
	}
	setFlash(w, "ok", msg)
	redirect(w, r, s.siteURL(site)+"/settings#autodeploy")
}

// removeWebhook deletes the site's GitHub webhook, if it has one.
func (s *Server) removeWebhook(ctx context.Context, site store.Site) error {
	if site.HookID == 0 {
		return nil
	}
	token := s.githubToken()
	if token == "" {
		return errors.New("GitHub isn't connected")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return github.DeleteHook(ctx, token, site.Repo, site.HookID)
}

// pushEvent is the part of GitHub's push payload Skiff uses.
type pushEvent struct {
	Ref     string `json:"ref"`
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`
	Pusher  struct {
		Name string `json:"name"`
	} `json:"pusher"`
}

// githubWebhook receives GitHub deliveries. It is public (GitHub can't log
// in), so every request must carry a valid HMAC signature for the site.
func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	reply := func(code int, msg string) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(code)
		fmt.Fprintln(w, msg)
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		reply(http.StatusNotFound, "unknown site")
		return
	}
	site, err := s.st.Site(id)
	if err != nil || !site.AutoDeploy || site.HookSecret == "" {
		reply(http.StatusNotFound, "auto-deploy is not enabled for this site")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		reply(http.StatusRequestEntityTooLarge, "payload too large")
		return
	}
	if !github.ValidSignature(site.HookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		slog.Warn("webhook with a bad signature", "site", site.Domain, "ip", clientIP(r))
		reply(http.StatusUnauthorized, "bad signature")
		return
	}

	switch event := r.Header.Get("X-GitHub-Event"); event {
	case "ping":
		reply(http.StatusOK, "pong")
		return
	case "push":
	default:
		reply(http.StatusOK, "ignored "+event+" event")
		return
	}
	var p pushEvent
	if err := json.Unmarshal(body, &p); err != nil {
		reply(http.StatusBadRequest, "invalid JSON")
		return
	}
	if p.Deleted || p.Ref != "refs/heads/"+site.Branch {
		reply(http.StatusOK, "ignored: not a push to "+site.Branch)
		return
	}
	if site.Status == store.SiteDeleting {
		reply(http.StatusConflict, "site is being deleted")
		return
	}
	// A queued deploy will fetch this commit anyway.
	if queued, _ := s.st.HasQueuedJob(site.ID, "deploy"); queued {
		reply(http.StatusOK, "a deploy is already queued; it will include this push")
		return
	}
	commit := p.After
	if len(commit) > 7 {
		commit = commit[:7]
	}
	reason := "push " + commit
	if p.Pusher.Name != "" {
		reason += " by " + p.Pusher.Name
	}
	jobID, err := s.startDeploy(site, reason)
	if err != nil {
		slog.Error("queue deploy from webhook", "site", site.Domain, "err", err)
		reply(http.StatusInternalServerError, "could not queue deploy")
		return
	}
	slog.Info("deploy queued by push", "site", site.Domain, "commit", commit, "job", jobID)
	reply(http.StatusAccepted, "deploy queued (job "+strconv.FormatInt(jobID, 10)+")")
}
