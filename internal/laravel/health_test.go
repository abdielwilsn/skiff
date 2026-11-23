package laravel

import (
	"testing"
	"time"
)

const healthOutput = `cpu_a=1000 800
cpu_b=1200 900
cpus=2
load=0.52 0.40 0.31
memtotal=2048000000
memavailable=512000000
swaptotal=1073741824
swapfree=1073741824
disk=50000000000 45000000000 5000000000
uptime=93784.21
hostname=web-1
kernel=6.8.0-45-generic
os=Ubuntu 24.04.1 LTS
svc=nginx.service active
svc=php8.4-fpm.service active
svc=mysql.service failed
unit=skiff-app.example.com-worker@1.service active
unit=skiff-app.example.com-app.service failed
`

func TestParseHealth(t *testing.T) {
	h := parseHealth(healthOutput)
	if h.CPUs != 2 || h.CPUPercent != 50 || h.Load1 != 0.52 || h.Hostname != "web-1" || h.OS != "Ubuntu 24.04.1 LTS" {
		t.Errorf("basics: %+v", h)
	}
	if m := h.MemUsedPercent(); m != 75 {
		t.Errorf("memory used = %v", m)
	}
	if dsk := h.DiskUsedPercent(); dsk != 90 {
		t.Errorf("disk used = %v", dsk)
	}
	if h.Uptime != 93784*time.Second {
		t.Errorf("uptime = %v", h.Uptime)
	}
	if len(h.Services) != 3 || h.Services[0].Name != "nginx" || len(h.SiteUnits) != 2 {
		t.Errorf("units: %+v %+v", h.Services, h.SiteUnits)
	}
	if f := h.FailedUnits(); len(f) != 2 || f[0].Name != "mysql" {
		t.Errorf("failed = %+v", f)
	}
	if empty := parseHealth("[dry-run] as (panel user)\n  | cpu() {"); empty.CPUPercent != 0 || empty.MemTotal != 0 {
		t.Errorf("garbage parsed into %+v", empty)
	}
}
