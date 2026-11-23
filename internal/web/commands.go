package web

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"

	"skiff/internal/laravel"
	"skiff/internal/store"
)

// commandPreset is a one-click command on the Commands page.
type commandPreset struct {
	Label, Command string
	Danger         bool // destroys data; the page asks for confirmation
}

var jsCommandPresets = []commandPreset{
	{"Node version", "node -v && npm -v", false},
	{"List scripts", "npm run", false},
	{"Outdated packages", "npm outdated || true", false},
	{"Installed packages", "npm ls --depth=0", false},
	{"Disk usage", "du -sh . node_modules 2>/dev/null", false},
}

var pythonCommandPresets = []commandPreset{
	{"Python version", "python --version && uv --version", false},
	{"Installed packages", "uv pip list", false},
	{"Outdated packages", "uv pip list --outdated", false},
	{"Django migrate", "python manage.py migrate --noinput", false},
	{"Django migrations", "python manage.py showmigrations", false},
	{"Django check", "python manage.py check --deploy", false},
	{"Collect static", "python manage.py collectstatic --noinput", false},
	{"Disk usage", "du -sh . .venv 2>/dev/null", false},
}

// presetsFor returns the one-click commands for a site's project type.
func presetsFor(site store.Site) []commandPreset {
	switch {
	case site.IsLaravel():
		return commandPresets
	case site.IsPython():
		return pythonCommandPresets
	}
	return jsCommandPresets
}

var commandPresets = []commandPreset{
	{"Migrate", "php artisan migrate --force", false},
	{"Migration status", "php artisan migrate:status", false},
	{"Seed", "php artisan db:seed --force", false},
	{"Rollback last batch", "php artisan migrate:rollback --force", true},
	{"Fresh + seed", "php artisan migrate:fresh --seed --force", true},
	{"Restart queue workers", "php artisan queue:restart", false},
	{"Clear caches", "php artisan optimize:clear", false},
	{"Re-cache", "php artisan optimize", false},
	{"Storage link", "php artisan storage:link", false},
	{"About", "php artisan about", false},
}

type commandsData struct {
	Site    store.Site
	Presets []commandPreset
	History []store.Job
	Command string
	Error   string
}

func (s *Server) commandsPage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	d := commandsData{Site: site, Presets: presetsFor(site), Command: r.URL.Query().Get("command")}
	d.History, _ = s.st.SiteJobsOfKind(site.ID, "command", 20)
	s.render(w, r, "site_commands", http.StatusOK, sitePageFor(sess, site, "commands", "Commands", d))
}

func (s *Server) runCommand(w http.ResponseWriter, r *http.Request, sess store.Session) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	command := strings.TrimSpace(strings.ReplaceAll(r.FormValue("command"), "\r\n", "\n"))
	fail := func(msg string) {
		d := commandsData{Site: site, Presets: presetsFor(site), Command: command, Error: msg}
		d.History, _ = s.st.SiteJobsOfKind(site.ID, "command", 20)
		s.render(w, r, "site_commands", http.StatusUnprocessableEntity, sitePageFor(sess, site, "commands", "Commands", d))
	}
	switch {
	case command == "":
		fail("Enter a command to run.")
		return
	case len(command) > laravel.MaxCommandLen:
		fail("That command is too long.")
		return
	case site.DeployedAt == nil:
		fail("Deploy the site once before running commands in it.")
		return
	}
	siteID := site.ID
	id, err := s.runner.EnqueueDetail(&siteID, "command", "Run "+commandTitle(command), command,
		func(ctx context.Context, log io.Writer) error {
			cur, err := s.st.Site(siteID) // current PHP/Node versions
			if err != nil {
				return err
			}
			return s.deployer.RunCommand(ctx, log, cur, command)
		}, nil)
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirect(w, r, "/jobs/"+strconv.FormatInt(id, 10))
}

// commandTitle is a one-line, length-limited label for a command.
func commandTitle(command string) string {
	first, _, multi := strings.Cut(command, "\n")
	first = strings.TrimSpace(first)
	if len(first) > 72 {
		first = first[:69] + "…"
	} else if multi {
		first += " …"
	}
	return first
}
