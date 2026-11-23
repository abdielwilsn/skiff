package laravel

import (
	"bytes"
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"skiff/internal/executor"
)

// Health is a snapshot of the server's resources and services, read from
// /proc, df and systemd in one short script (about half a second, most of it
// spent measuring CPU use).
type Health struct {
	Hostname, OS, Kernel string
	CPUs                 int
	CPUPercent           float64 // busy share of all cores over the measurement
	Load1, Load5, Load15 float64
	MemTotal, MemAvail   int64 // bytes
	SwapTotal, SwapFree  int64
	DiskTotal, DiskUsed  int64 // the root filesystem
	DiskAvail            int64
	Uptime               time.Duration
	Services             []Unit // nginx, PHP-FPM, MySQL
	SiteUnits            []Unit // skiff-<domain>-* app servers, workers, timers
}

// Unit is a systemd unit and its state ("active", "failed", "inactive"…).
type Unit struct{ Name, State string }

func (h Health) MemUsedPercent() float64  { return pct(h.MemTotal-h.MemAvail, h.MemTotal) }
func (h Health) SwapUsedPercent() float64 { return pct(h.SwapTotal-h.SwapFree, h.SwapTotal) }
func (h Health) DiskUsedPercent() float64 { return pct(h.DiskUsed, h.DiskUsed+h.DiskAvail) }

func pct(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) * 100 / float64(whole)
}

// FailedUnits lists services and site processes that aren't running.
func (h Health) FailedUnits() []Unit {
	var out []Unit
	for _, u := range append(append([]Unit{}, h.Services...), h.SiteUnits...) {
		if u.State == "failed" {
			out = append(out, u)
		}
	}
	return out
}

const healthScript = `cpu() { awk '/^cpu / {t=0; for (i=2;i<=9;i++) t+=$i; print t, $5+$6}' /proc/stat; }
echo "cpu_a=$(cpu)"
sleep 0.5
echo "cpu_b=$(cpu)"
echo "cpus=$(nproc 2>/dev/null || echo 1)"
echo "load=$(cut -d' ' -f1-3 /proc/loadavg)"
awk '/^(MemTotal|MemAvailable|SwapTotal|SwapFree):/ {gsub(":",""); print tolower($1) "=" $2*1024}' /proc/meminfo
echo "disk=$(df -B1 --output=size,used,avail / | tail -n 1)"
echo "uptime=$(cut -d' ' -f1 /proc/uptime)"
echo "hostname=$(hostname)"
echo "kernel=$(uname -r)"
. /etc/os-release 2>/dev/null && echo "os=$PRETTY_NAME"
svc() { if systemctl cat "$1" >/dev/null 2>&1; then echo "svc=$1 $(systemctl is-active "$1" 2>/dev/null)"; fi; }
svc nginx.service
svc mysql.service
for f in /lib/systemd/system/php*-fpm.service; do [ -e "$f" ] && svc "$(basename "$f")"; done
# Site processes; the scheduler's oneshot service is idle between timer runs.
systemctl list-units --all --plain --no-legend 'skiff-*' 2>/dev/null | awk '$1 !~ /-schedule\.service$/ {print "unit=" $1 " " $3}'
true`

// Health reads the server's current state.
func (d *Deployer) Health(ctx context.Context) (Health, error) {
	var out bytes.Buffer
	if err := d.Exec.Run(ctx, healthScript, executor.Options{}, &out); err != nil {
		return Health{}, err
	}
	return parseHealth(out.String()), nil
}

func parseHealth(s string) Health {
	var h Health
	var a, b []float64
	nums := func(v string) []float64 {
		var out []float64
		for _, f := range strings.Fields(v) {
			n, _ := strconv.ParseFloat(f, 64)
			out = append(out, n)
		}
		return out
	}
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "cpu_a":
			a = nums(v)
		case "cpu_b":
			b = nums(v)
		case "cpus":
			h.CPUs, _ = strconv.Atoi(v)
		case "load":
			if n := nums(v); len(n) == 3 {
				h.Load1, h.Load5, h.Load15 = n[0], n[1], n[2]
			}
		case "memtotal":
			h.MemTotal, _ = strconv.ParseInt(v, 10, 64)
		case "memavailable":
			h.MemAvail, _ = strconv.ParseInt(v, 10, 64)
		case "swaptotal":
			h.SwapTotal, _ = strconv.ParseInt(v, 10, 64)
		case "swapfree":
			h.SwapFree, _ = strconv.ParseInt(v, 10, 64)
		case "disk":
			if n := nums(v); len(n) == 3 {
				h.DiskTotal, h.DiskUsed, h.DiskAvail = int64(n[0]), int64(n[1]), int64(n[2])
			}
		case "uptime":
			if n := nums(v); len(n) == 1 {
				h.Uptime = time.Duration(n[0]) * time.Second
			}
		case "hostname":
			h.Hostname = v
		case "kernel":
			h.Kernel = v
		case "os":
			h.OS = v
		case "svc", "unit":
			name, state, _ := strings.Cut(v, " ")
			if name == "" {
				continue
			}
			u := Unit{Name: strings.TrimSuffix(name, ".service"), State: strings.TrimSpace(state)}
			if k == "svc" {
				h.Services = append(h.Services, u)
			} else {
				h.SiteUnits = append(h.SiteUnits, u)
			}
		}
	}
	if len(a) == 2 && len(b) == 2 && b[0] > a[0] {
		busy := (b[0] - a[0]) - (b[1] - a[1])
		h.CPUPercent = busy * 100 / (b[0] - a[0])
		if h.CPUPercent < 0 {
			h.CPUPercent = 0
		}
	}
	sort.Slice(h.SiteUnits, func(i, j int) bool { return h.SiteUnits[i].Name < h.SiteUnits[j].Name })
	return h
}
