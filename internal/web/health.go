package web

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"skiff/internal/laravel"
	"skiff/internal/store"
)

// Server health: Skiff samples CPU, memory and disk once a minute and keeps
// the last 24 hours in memory (about 70 KB), so the Server page can chart
// them without a database or an agent.

const (
	healthEvery   = time.Minute
	healthHistory = 24 * time.Hour
	diskWarnPct   = 85.0
	diskCritPct   = 95.0
	memWarnPct    = 90.0
)

type healthSample struct {
	T              time.Time
	CPU, Mem, Disk float64
}

// healthLoop samples the server once a minute.
func (s *Server) healthLoop(ctx context.Context) {
	if s.cfg.DryRun {
		return // the dry-run executor doesn't run anything to measure
	}
	t := time.NewTicker(healthEvery)
	defer t.Stop()
	for {
		s.sampleHealth(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) sampleHealth(ctx context.Context) {
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	h, err := s.deployer.Health(c)
	if err != nil {
		slog.Warn("read server health", "err", err)
		return
	}
	s.recordHealth(h, time.Now())
}

func (s *Server) recordHealth(h laravel.Health, now time.Time) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	s.healthNow = &h
	s.healthHist = append(s.healthHist, healthSample{T: now, CPU: h.CPUPercent, Mem: h.MemUsedPercent(), Disk: h.DiskUsedPercent()})
	cut := 0
	for cut < len(s.healthHist) && now.Sub(s.healthHist[cut].T) > healthHistory {
		cut++
	}
	if cut > 0 {
		s.healthHist = append([]healthSample(nil), s.healthHist[cut:]...)
	}
}

func (s *Server) healthSnapshot() (*laravel.Health, []healthSample) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	return s.healthNow, append([]healthSample(nil), s.healthHist...)
}

// healthAlert is a problem worth showing on the dashboard.
type healthAlert struct {
	Tone, Msg string // Tone: "warn" or "err"
}

func healthAlerts(h *laravel.Health) []healthAlert {
	if h == nil {
		return nil
	}
	var out []healthAlert
	if d := h.DiskUsedPercent(); d >= diskCritPct {
		out = append(out, healthAlert{"err", fmt.Sprintf("The disk is %.0f%% full (%s free). Deploys, backups and databases fail when it fills up.", d, humanBytes(h.DiskAvail))})
	} else if d >= diskWarnPct {
		out = append(out, healthAlert{"warn", fmt.Sprintf("The disk is %.0f%% full (%s free).", d, humanBytes(h.DiskAvail))})
	}
	if m := h.MemUsedPercent(); m >= memWarnPct {
		out = append(out, healthAlert{"warn", fmt.Sprintf("Memory is %.0f%% used (%s available).", m, humanBytes(h.MemAvail))})
	}
	if f := h.FailedUnits(); len(f) > 0 {
		names := make([]string, len(f))
		for i, u := range f {
			names[i] = u.Name
		}
		out = append(out, healthAlert{"err", "Not running: " + strings.Join(names, ", ") + "."})
	}
	return out
}

// ---- charts -------------------------------------------------------------------

// chart is a single-series line chart of a percentage over time, drawn as SVG.
type chart struct {
	ID, Title      string
	Now, Peak, Avg float64
	Line, Area     string // SVG path data
	Data           string // "x:y:unix:value;…" for the hover layer
	Start, End     string // x-axis labels
	Warn, WarnY    float64
	Empty          bool
}

const (
	chartW   = 640
	chartH   = 150
	chartTop = 8.0
	chartBot = 142.0
)

func chartY(v float64) float64 {
	v = math.Max(0, math.Min(100, v))
	return chartBot - v/100*(chartBot-chartTop)
}

func buildChart(id, title string, hist []healthSample, now time.Time, warn float64, pick func(healthSample) float64) chart {
	c := chart{ID: id, Title: title, Warn: warn, WarnY: chartY(warn)}
	if len(hist) == 0 {
		c.Empty = true
		return c
	}
	start := hist[0].T
	if now.Sub(start) < time.Hour {
		start = now.Add(-time.Hour)
	}
	span := now.Sub(start).Seconds()
	var line, data strings.Builder
	var sum float64
	firstX, lastX := 0.0, 0.0
	for i, p := range hist {
		v := pick(p)
		sum += v
		if v > c.Peak {
			c.Peak = v
		}
		x := p.T.Sub(start).Seconds() / span * chartW
		if i == 0 {
			firstX = x
			fmt.Fprintf(&line, "M%.1f %.1f", x, chartY(v))
		} else {
			fmt.Fprintf(&line, " L%.1f %.1f", x, chartY(v))
		}
		lastX = x
		fmt.Fprintf(&data, "%.1f:%.1f:%d:%.1f;", x, chartY(v), p.T.Unix(), v)
	}
	c.Now = pick(hist[len(hist)-1])
	c.Avg = sum / float64(len(hist))
	c.Line = line.String()
	c.Area = fmt.Sprintf("%s L%.1f %.1f L%.1f %.1f Z", c.Line, lastX, chartBot, firstX, chartBot)
	c.Data = strings.TrimSuffix(data.String(), ";")
	c.Start = start.Format("15:04")
	c.End = "now"
	return c
}

// ---- page ---------------------------------------------------------------------

type serverData struct {
	Health  *laravel.Health
	Err     string
	Alerts  []healthAlert
	Charts  []chart
	Samples int
	DryRun  bool
	Version string
}

func (s *Server) serverPage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	d := serverData{DryRun: s.cfg.DryRun, Version: s.cfg.Version}
	if !s.cfg.DryRun {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		h, err := s.deployer.Health(ctx)
		cancel()
		if err != nil {
			d.Err = "Couldn't read the server's state: " + err.Error()
		} else {
			d.Health = &h
			s.recordHealth(h, time.Now())
		}
	}
	d.Alerts = healthAlerts(d.Health)
	_, hist := s.healthSnapshot()
	d.Samples = len(hist)
	now := time.Now()
	d.Charts = []chart{
		buildChart("cpu", "CPU", hist, now, 90, func(p healthSample) float64 { return p.CPU }),
		buildChart("mem", "Memory", hist, now, memWarnPct, func(p healthSample) float64 { return p.Mem }),
		buildChart("disk", "Disk", hist, now, diskWarnPct, func(p healthSample) float64 { return p.Disk }),
	}
	s.render(w, r, "server", http.StatusOK, pageFor(sess, "Server", "server", d))
}
