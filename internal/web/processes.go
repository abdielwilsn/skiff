package web

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"skiff/internal/laravel"
	"skiff/internal/store"
)

// saveAppSettings stores a Node app's start command or a static site's output
// folder, and applies it right away if the site is deployed.
func (s *Server) saveAppSettings(w http.ResponseWriter, r *http.Request, _ store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	start := strings.TrimSpace(r.FormValue("start_command"))
	out := strings.Trim(strings.TrimSpace(r.FormValue("output_dir")), "/")
	fail := func(msg string) {
		setFlash(w, "err", msg)
		redirect(w, r, s.siteURL(site)+"#app")
	}
	switch {
	case site.HasApp() && !laravel.ValidStartCommand(start):
		fail("Enter the command that starts your app, e.g. npm run start.")
		return
	case site.IsStatic() && !laravel.ValidOutputDir(out):
		fail("Enter the folder your build writes to, e.g. dist (or . for the repository root).")
		return
	case site.IsLaravel():
		fail("Laravel sites don't have app settings.")
		return
	}
	if site.HasApp() {
		out = site.OutputDir
	} else {
		start = site.StartCommand
	}
	if err := s.st.SetAppSettings(site.ID, start, out); err != nil {
		s.serverError(w, err)
		return
	}
	if site.DeployedAt == nil {
		setFlash(w, "ok", "Saved. It's used on the first deploy.")
		redirect(w, r, s.siteURL(site)+"#app")
		return
	}
	s.enqueueSiteJob(w, r, site, "app", "Apply app settings for "+site.Domain, func(ctx context.Context, log io.Writer, cur store.Site) error {
		return s.deployer.ApplyAppSettings(ctx, log, cur)
	})
}

// saveWorkers stores a Laravel site's queue workers and scheduler and applies them.
func (s *Server) saveWorkers(w http.ResponseWriter, r *http.Request, _ store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	count, err := strconv.Atoi(strings.TrimSpace(r.FormValue("workers")))
	opts := strings.Join(strings.Fields(r.FormValue("options")), " ")
	scheduler := r.FormValue("scheduler") == "1"
	fail := func(msg string) {
		setFlash(w, "err", msg)
		redirect(w, r, s.siteURL(site)+"#workers")
	}
	switch {
	case !site.IsLaravel():
		fail("Queue workers and the scheduler are for Laravel sites.")
		return
	case err != nil || count < 0 || count > laravel.MaxWorkers:
		fail("Choose between 0 and " + strconv.Itoa(laravel.MaxWorkers) + " workers.")
		return
	case !laravel.ValidWorkerOptions(opts):
		fail("Worker options may contain letters, digits, spaces and = , . : / - _ only.")
		return
	}
	if err := s.st.SetWorkers(site.ID, count, opts, scheduler); err != nil {
		s.serverError(w, err)
		return
	}
	if site.DeployedAt == nil {
		setFlash(w, "ok", "Saved. Workers and the scheduler start after the first deploy.")
		redirect(w, r, s.siteURL(site)+"#workers")
		return
	}
	s.enqueueSiteJob(w, r, site, "workers", "Update queue workers for "+site.Domain, func(ctx context.Context, log io.Writer, cur store.Site) error {
		return s.deployer.ApplyLaravelProcesses(ctx, log, cur)
	})
}

// restartProcesses restarts a Node or Python app, or a Laravel site's queue workers.
func (s *Server) restartProcesses(w http.ResponseWriter, r *http.Request, _ store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	if site.DeployedAt == nil {
		setFlash(w, "err", "Deploy the site first.")
		redirect(w, r, s.siteURL(site))
		return
	}
	title := "Restart " + site.Domain
	if site.IsLaravel() {
		title = "Restart queue workers for " + site.Domain
	}
	s.enqueueSiteJob(w, r, site, "restart", title, func(ctx context.Context, log io.Writer, cur store.Site) error {
		if cur.HasApp() {
			return s.deployer.StartApp(ctx, log, cur)
		}
		return s.deployer.ApplyLaravelProcesses(ctx, log, cur)
	})
}

// enqueueSiteJob runs fn for the site (reloaded when the job starts) and
// redirects to the job's log.
func (s *Server) enqueueSiteJob(w http.ResponseWriter, r *http.Request, site store.Site, kind, title string,
	fn func(ctx context.Context, log io.Writer, cur store.Site) error) {
	if busy, _ := s.st.HasActiveJob(site.ID); busy {
		setFlash(w, "err", "Wait for the running job to finish first.")
		redirect(w, r, s.siteURL(site))
		return
	}
	siteID := site.ID
	id, err := s.runner.Enqueue(&siteID, kind, title, func(ctx context.Context, log io.Writer) error {
		cur, err := s.st.Site(siteID)
		if err != nil {
			return err
		}
		return fn(ctx, log, cur)
	}, nil)
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(id, 10))
}

type logsData struct {
	Site  store.Site
	Logs  string
	Error string
}

func (s *Server) logsPage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	d := logsData{Site: site}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	logs, err := s.deployer.Logs(ctx, site)
	if err != nil {
		d.Error = "Couldn't read logs: " + err.Error()
	}
	d.Logs = logs
	s.render(w, r, "site_logs", http.StatusOK, sitePageFor(sess, site, "logs", "Logs", d))
}
