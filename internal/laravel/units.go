package laravel

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"skiff/internal/executor"
	"skiff/internal/store"
)

// Long-running processes are systemd units, so they start on boot, restart
// when they crash, and log to the journal:
//
//	skiff-<domain>-worker@N.service   Laravel queue workers (N = 1..count)
//	skiff-<domain>-schedule.timer     Laravel scheduler, every minute
//	skiff-<domain>-app.service        Node.js app server
//
// All run as the site user from the live release ("current").

const unitDir = "/etc/systemd/system"

func unitPrefix(domain string) string { return "skiff-" + domain }
func appUnit(domain string) string    { return unitPrefix(domain) + "-app.service" }
func appStartScript(domain string) string {
	return "/usr/local/lib/skiff/" + domain + "-start.sh"
}

// runtimePath is PATH with the site's PHP and Node.js first.
func runtimePath(site store.Site) string {
	var parts []string
	if ValidPHP(site.PHPVersion) && site.IsLaravel() {
		parts = append(parts, phpShimDir(site.PHPVersion))
	}
	if site.NodeVersion != "" && ValidNode(site.NodeVersion) {
		parts = append(parts, nodeDir(site.NodeVersion)+"/bin")
	}
	parts = append(parts, "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin")
	return strings.Join(parts, ":")
}

// ApplyLaravelProcesses makes the site's queue workers and scheduler match its
// settings: (re)writes the units, starts or restarts what's enabled, and
// stops and removes what isn't. Safe to run on every deploy.
func (d *Deployer) ApplyLaravelProcesses(ctx context.Context, w io.Writer, site store.Site) error {
	if !ValidWorkerOptions(site.WorkerOptions) {
		return fmt.Errorf("invalid queue worker options")
	}
	count := site.WorkerCount
	if count < 0 || count > MaxWorkers {
		return fmt.Errorf("queue workers must be between 0 and %d", MaxWorkers)
	}
	p := unitPrefix(site.Domain)
	step(w, "Queue workers: %d · scheduler: %s", count, map[bool]string{true: "on", false: "off"}[site.Scheduler])
	script := locate(site.Domain) + fmt.Sprintf(`P=%[1]s
D=%[2]s
WANTS=%[3]s/multi-user.target.wants
COUNT=%[4]d

if [ "$COUNT" -gt 0 ]; then
    cat > %[3]s/$P-worker@.service <<EOF
# Managed by Skiff
[Unit]
Description=Skiff queue worker %%i for $D
After=network.target

[Service]
Type=simple
User=%[5]s
Group=%[5]s
WorkingDirectory=$APP
Environment=PATH=%[6]s
ExecStart=/usr/bin/env php artisan queue:work %[7]s
Restart=always
RestartSec=3
KillSignal=SIGTERM
TimeoutStopSec=120

[Install]
WantedBy=multi-user.target
EOF
fi

if [ %[8]t = true ]; then
    cat > %[3]s/$P-schedule.service <<EOF
# Managed by Skiff
[Unit]
Description=Skiff scheduler for $D

[Service]
Type=oneshot
User=%[5]s
Group=%[5]s
WorkingDirectory=$APP
Environment=PATH=%[6]s
ExecStart=/usr/bin/env php artisan schedule:run
EOF
    cat > %[3]s/$P-schedule.timer <<EOF
# Managed by Skiff
[Unit]
Description=Run the Laravel scheduler for $D every minute

[Timer]
OnCalendar=*-*-* *:*:00
AccuracySec=1s

[Install]
WantedBy=timers.target
EOF
fi
systemctl daemon-reload

# Workers 1..COUNT run (restarted, so they load the latest release); others stop.
for i in $(seq 1 "$COUNT"); do
    systemctl enable "$P-worker@$i.service" >/dev/null 2>&1
    systemctl restart "$P-worker@$i.service"
done
shopt -s nullglob
for link in "$WANTS"/$P-worker@*.service; do
    n="${link##*@}"; n="${n%%.service}"
    if [ "$n" -gt "$COUNT" ] 2>/dev/null; then
        systemctl disable --now "$P-worker@$n.service" >/dev/null 2>&1 || true
    fi
done
if [ "$COUNT" -eq 0 ]; then
    systemctl stop "$P-worker@*.service" 2>/dev/null || true
    rm -f %[3]s/$P-worker@.service
fi

if [ %[8]t = true ]; then
    systemctl enable --now "$P-schedule.timer" >/dev/null 2>&1
else
    systemctl disable --now "$P-schedule.timer" >/dev/null 2>&1 || true
    rm -f %[3]s/$P-schedule.service %[3]s/$P-schedule.timer
fi
systemctl daemon-reload
echo "Workers running: $(systemctl list-units --plain --no-legend --state=active "$P-worker@*.service" | wc -l)/$COUNT, scheduler: $(systemctl is-active "$P-schedule.timer" 2>/dev/null || true)"`,
		p, site.Domain, unitDir, count, SiteUser, runtimePath(site), strings.TrimSpace(site.WorkerOptions), site.Scheduler)
	return d.run(ctx, w, script, executor.Options{})
}

// StartApp writes a Node app's start script and unit, (re)starts it, and
// waits until it answers on its port.
func (d *Deployer) StartApp(ctx context.Context, w io.Writer, site store.Site) error {
	if !site.IsNode() || site.Port <= 0 {
		return fmt.Errorf("not a Node.js app")
	}
	if !ValidStartCommand(site.StartCommand) {
		return fmt.Errorf("set a start command for the app")
	}
	step(w, "Starting the app: %s", site.StartCommand)
	// The start script exports PORT/HOST last, so a PORT in .env can't move the
	// app away from the port Nginx proxies to.
	start := fmt.Sprintf(`#!/bin/bash
# Managed by Skiff: starts %[1]s
export PATH=%[2]s
export NODE_ENV="${NODE_ENV:-production}"
export PORT=%[3]d HOST=127.0.0.1 HOSTNAME=127.0.0.1
exec %[4]s
`, site.Domain, runtimePath(site), site.Port, strings.TrimSpace(site.StartCommand))
	script := locate(site.Domain) + fmt.Sprintf(`mkdir -p /usr/local/lib/skiff
cat > %[1]s
chmod 755 %[1]s
cat > %[2]s/%[3]s <<EOF
# Managed by Skiff
[Unit]
Description=Skiff app %[4]s
After=network.target

[Service]
Type=simple
User=%[5]s
Group=%[5]s
WorkingDirectory=$APP
EnvironmentFile=-$SITE/shared/.env
ExecStart=%[1]s
Restart=always
RestartSec=3
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable %[3]s >/dev/null 2>&1
systemctl restart %[3]s
for i in $(seq 1 60); do
    code="$(curl -s -o /dev/null -w '%%{http_code}' --max-time 2 http://127.0.0.1:%[6]d/ || true)"
    if [ "$code" != "000" ]; then echo "App is up on port %[6]d (HTTP $code)"; exit 0; fi
    sleep 1
done
echo "The app didn't answer on port %[6]d within 60 seconds. Last log lines:" >&2
journalctl -u %[3]s -n 25 --no-pager -o cat >&2 || true
exit 1`, appStartScript(site.Domain), unitDir, appUnit(site.Domain), site.Domain, SiteUser, site.Port)
	return d.run(ctx, w, script, executor.Options{Stdin: strings.NewReader(start)})
}

// RemoveProcesses stops and deletes every unit belonging to the site.
func (d *Deployer) RemoveProcesses(ctx context.Context, w io.Writer, domain string) error {
	p := unitPrefix(domain)
	script := fmt.Sprintf(`P=%[1]s
shopt -s nullglob
for u in %[2]s/$P-*.service %[2]s/$P-*.timer %[2]s/multi-user.target.wants/$P-* %[2]s/timers.target.wants/$P-*; do
    systemctl disable --now "$(basename "$u")" >/dev/null 2>&1 || true
done
systemctl stop "$P-*" 2>/dev/null || true
rm -f %[2]s/$P-*.service %[2]s/$P-*.timer %[3]s
systemctl daemon-reload
echo "Stopped background processes"`, p, unitDir, appStartScript(domain))
	return d.run(ctx, w, script, executor.Options{})
}

// ProcessStatus is the live state of a site's background processes.
type ProcessStatus struct {
	App              string // systemd state of the Node app ("active", "failed", …)
	WorkersRunning   int
	SchedulerEnabled bool
}

// Status reads the state of the site's units.
func (d *Deployer) Status(ctx context.Context, site store.Site) (ProcessStatus, error) {
	p := unitPrefix(site.Domain)
	script := fmt.Sprintf(`echo "app=$(systemctl is-active %[1]s-app.service 2>/dev/null || true)"
echo "workers=$(systemctl list-units --plain --no-legend --state=active '%[1]s-worker@*.service' 2>/dev/null | wc -l)"
echo "timer=$(systemctl is-active %[1]s-schedule.timer 2>/dev/null || true)"`, p)
	var out bytes.Buffer
	var st ProcessStatus
	if err := d.Exec.Run(ctx, script, executor.Options{}, &out); err != nil {
		return st, err
	}
	for _, line := range strings.Split(out.String(), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "app":
			st.App = v
		case "workers":
			st.WorkersRunning, _ = strconv.Atoi(strings.TrimSpace(v))
		case "timer":
			st.SchedulerEnabled = v == "active"
		}
	}
	return st, nil
}

// Logs returns recent output of the site's processes and, for Laravel, the
// tail of storage/logs/laravel.log.
func (d *Deployer) Logs(ctx context.Context, site store.Site) (string, error) {
	p := unitPrefix(site.Domain)
	script := locate(site.Domain) + fmt.Sprintf(`echo "==> Background processes (journal, newest last)"
journalctl --no-pager -n 200 -o short-iso -u '%[1]s-*' 2>/dev/null | tail -n 200 || true
LOG="$SITE/shared/storage/logs/laravel.log"
[ -f "$LOG" ] || LOG="$APP/storage/logs/laravel.log"
if [ -f "$LOG" ]; then
    echo
    echo "==> $LOG (last 150 lines)"
    tail -n 150 "$LOG"
fi`, p)
	var out bytes.Buffer
	err := d.Exec.Run(ctx, script, executor.Options{}, &out)
	return out.String(), err
}

// ApplyAppSettings re-applies a site's Nginx config (e.g. a static site's
// output folder) and, for a Node app, restarts it with its start command.
func (d *Deployer) ApplyAppSettings(ctx context.Context, w io.Writer, site store.Site) error {
	if err := d.configureNginx(ctx, w, site); err != nil {
		return err
	}
	if site.IsNode() {
		return d.StartApp(ctx, w, site)
	}
	return nil
}
