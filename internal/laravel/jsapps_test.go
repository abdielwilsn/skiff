package laravel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"skiff/internal/executor"
	"skiff/internal/store"
)

var nodeSite = store.Site{ID: 1, Domain: "app.example.com", Repo: "o/next", Branch: "main", NodeVersion: "24",
	Kind: store.KindNode, Port: 4101, StartCommand: "npm run start", DeployScript: DefaultJSDeployScript}

var staticSite = store.Site{ID: 2, Domain: "spa.example.com", Repo: "o/spa", Branch: "main", NodeVersion: "24",
	Kind: store.KindStatic, OutputDir: "dist", DeployScript: DefaultJSDeployScript}

func TestFrameworkPresetsAreValid(t *testing.T) {
	for _, f := range Frameworks {
		switch f.Kind {
		case store.KindNode, store.KindPython:
			if !ValidStartCommand(f.Start) {
				t.Errorf("%s: invalid start command %q", f.ID, f.Start)
			}
		case store.KindStatic:
			if !ValidOutputDir(f.Output) {
				t.Errorf("%s: invalid output dir %q", f.ID, f.Output)
			}
		}
	}
	for _, bad := range []string{"../etc", "/var/www", "dist/../../x", "a b", ""} {
		if ValidOutputDir(bad) {
			t.Errorf("ValidOutputDir(%q) = true", bad)
		}
	}
	for _, bad := range []string{"", "npm start\nrm -rf /", strings.Repeat("x", 501)} {
		if ValidStartCommand(bad) {
			t.Errorf("ValidStartCommand(%q) = true", bad)
		}
	}
	for _, bad := range []string{"--queue=$(id)", "--tries=3; rm -rf /", "`id`", "--x \"y\""} {
		if ValidWorkerOptions(bad) {
			t.Errorf("ValidWorkerOptions(%q) = true", bad)
		}
	}
}

func TestNginxConfigPerKind(t *testing.T) {
	if c := NginxConfig(nodeSite); !strings.Contains(c, "proxy_pass http://127.0.0.1:4101;") || strings.Contains(c, "fastcgi") {
		t.Errorf("node config:\n%s", c)
	}
	if c := NginxConfig(staticSite); !strings.Contains(c, "root /home/skiff/spa.example.com/current/dist;") || !strings.Contains(c, "/index.html;") {
		t.Errorf("static config:\n%s", c)
	}
	plain := staticSite
	plain.OutputDir = "."
	if c := NginxConfig(plain); !strings.Contains(c, "root /home/skiff/spa.example.com/current;") {
		t.Errorf("plain HTML root:\n%s", c)
	}
}

func TestDeployNodeApp(t *testing.T) {
	rec := &recorder{}
	if err := (&Deployer{Exec: rec}).Deploy(context.Background(), &bytes.Buffer{}, nodeSite, "", nil); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(rec.scripts, "\n----\n")
	stdin := strings.Join(rec.stdin, "\n----\n")
	for _, want := range []string{"skiff-app.example.com-app.service", "ExecStart=/usr/local/lib/skiff/app.example.com-start.sh", "EnvironmentFile=-$SITE/shared/.env", "http://127.0.0.1:4101/", "STRIP=false"} {
		if !strings.Contains(all, want) {
			t.Errorf("node deploy scripts missing %q", want)
		}
	}
	for _, want := range []string{"export PORT=4101 HOST=127.0.0.1", "exec npm run start", "proxy_pass http://127.0.0.1:4101;"} {
		if !strings.Contains(stdin, want) {
			t.Errorf("node deploy inputs missing %q", want)
		}
	}
	if strings.Contains(all, `PHP="8.`) || strings.Contains(all, "-fpm reload") {
		t.Error("a Node app deploy must not install or reload PHP")
	}
}

// nodeExec reports a live release and fails the first app start.
type nodeExec struct {
	recorder
	starts int
}

func (e *nodeExec) Run(ctx context.Context, script string, opt executor.Options, w io.Writer) error {
	_ = e.recorder.Run(ctx, script, opt, w)
	if strings.HasPrefix(script, "readlink ") {
		io.WriteString(w, "releases/20260101000000\n")
	}
	if strings.Contains(script, "systemctl restart skiff-app.example.com-app.service") {
		e.starts++
		if e.starts == 1 {
			return errors.New("exit status 1")
		}
	}
	return nil
}

func TestNodeAppThatDoesNotStartIsRolledBack(t *testing.T) {
	e := &nodeExec{}
	err := (&Deployer{Exec: e}).Deploy(context.Background(), &bytes.Buffer{}, nodeSite, "", nil)
	if err == nil || !strings.Contains(err.Error(), "previous release (20260101000000) is live again") {
		t.Fatalf("want automatic rollback, got %v", err)
	}
	all := strings.Join(e.scripts, "\n----\n")
	if !strings.Contains(all, "'releases/20260101000000' current.next") {
		t.Error("did not switch back to the previous release")
	}
	if e.starts != 2 {
		t.Errorf("app started %d times, want 2 (new, then previous)", e.starts)
	}
}

func TestDeployStaticSite(t *testing.T) {
	rec := &recorder{}
	if err := (&Deployer{Exec: rec}).Deploy(context.Background(), &bytes.Buffer{}, staticSite, "", nil); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(rec.scripts, "\n----\n")
	if strings.Contains(all, "-app.service") || strings.Contains(all, "-fpm reload") || strings.Contains(all, `PHP="8.`) {
		t.Error("a static site must not start an app or touch PHP")
	}
	if !strings.Contains(all, `root /home/skiff/spa.example.com/current/dist;`) {
		t.Error("existing static configs should get their root updated to the output folder")
	}
	noNode := staticSite
	noNode.NodeVersion = ""
	if err := (&Deployer{Exec: &recorder{}}).Deploy(context.Background(), &bytes.Buffer{}, noNode, "", nil); err == nil {
		t.Error("JS sites need a Node.js version")
	}
}

func TestApplyLaravelProcesses(t *testing.T) {
	rec := &recorder{}
	site := store.Site{Domain: "app.example.com", PHPVersion: "8.4", NodeVersion: "24", Kind: store.KindLaravel,
		WorkerCount: 2, WorkerOptions: "redis --queue=high,default --tries=3", Scheduler: true}
	if err := (&Deployer{Exec: rec}).ApplyLaravelProcesses(context.Background(), &bytes.Buffer{}, site); err != nil {
		t.Fatal(err)
	}
	s := rec.scripts[0]
	for _, want := range []string{
		"P=skiff-app.example.com",
		"$P-worker@.service <<EOF",
		"ExecStart=/usr/bin/env php artisan queue:work redis --queue=high,default --tries=3",
		"WorkingDirectory=$APP",
		"Environment=PATH=/opt/skiff/php/8.4:/opt/skiff/node/24/bin:",
		"COUNT=2",
		"OnCalendar=*-*-* *:*:00",
		"ExecStart=/usr/bin/env php artisan schedule:run",
		`if [ true = true ]`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("worker script missing %q", want)
		}
	}
	site.WorkerOptions = "--x=$(id)"
	if err := (&Deployer{Exec: rec}).ApplyLaravelProcesses(context.Background(), &bytes.Buffer{}, site); err == nil {
		t.Error("unsafe worker options accepted")
	}
}
