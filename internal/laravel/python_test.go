package laravel

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"skiff/internal/store"
)

var djangoSite = store.Site{ID: 3, Domain: "dj.example.com", Repo: "o/dj", Branch: "main", PHPVersion: "8.4",
	Kind: store.KindPython, PythonVersion: "3.13", Port: 4103,
	StartCommand: FrameworkByID("django").Start, DeployScript: FrameworkByID("django").DeployScript()}

func TestPythonPresets(t *testing.T) {
	dj := FrameworkByID("django")
	if dj.Kind != store.KindPython || !strings.Contains(dj.DeployScript(), "uv pip install gunicorn") {
		t.Errorf("django preset = %+v", dj)
	}
	if s := FrameworkByID("python").DeployScript(); strings.Contains(s, "uv pip install gunicorn") || strings.Contains(s, "{{SERVER}}") {
		t.Errorf("generic Python script installs an app server or has a placeholder:\n%s", s)
	}
	if !strings.Contains(FrameworkByID("fastapi").DeployScript(), "uv pip install uvicorn") {
		t.Error("FastAPI preset should install uvicorn")
	}
	for _, v := range []string{"3.13", DefaultPython} {
		if !ValidPython(v) {
			t.Errorf("ValidPython(%q) = false", v)
		}
	}
	for _, v := range []string{"", "2.7", "3.13; rm -rf /"} {
		if ValidPython(v) {
			t.Errorf("ValidPython(%q) = true", v)
		}
	}
}

func TestPythonNginxConfig(t *testing.T) {
	c := NginxConfig(djangoSite)
	for _, want := range []string{"proxy_pass http://127.0.0.1:4103;", "alias /home/skiff/dj.example.com/current/staticfiles/;", "try_files $uri @app;", "location @app"} {
		if !strings.Contains(c, want) {
			t.Errorf("python nginx config missing %q:\n%s", want, c)
		}
	}
}

func TestDeployPythonApp(t *testing.T) {
	rec := &recorder{}
	if err := (&Deployer{Exec: rec}).Deploy(context.Background(), &bytes.Buffer{}, djangoSite, "", nil); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(rec.scripts, "\n----\n")
	stdin := strings.Join(rec.stdin, "\n----\n")
	for _, want := range []string{
		`VERSION="3.13"`, "uv python install", "astral-sh/uv/releases/latest/download", "sha256sum", // runtime
		`ln -s "$SH/db.sqlite3" db.sqlite3`, // Django's SQLite lives in shared/
		"UV_PYTHON=3.13", "UV_PYTHON_DOWNLOADS=never", "uv venv --quiet .venv", "python manage.py migrate --noinput",
		"skiff-dj.example.com-app.service", "http://127.0.0.1:4103/",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("python deploy scripts missing %q", want)
		}
	}
	for _, want := range []string{
		"export PATH=/home/skiff/dj.example.com/current/.venv/bin:/opt/skiff/uv:",
		"export VIRTUAL_ENV=/home/skiff/dj.example.com/current/.venv PYTHONUNBUFFERED=1",
		`WSGI_MODULE="$(dirname "$f").wsgi"`,
		`exec gunicorn "$WSGI_MODULE" --bind "$HOST:$PORT"`,
		"alias /home/skiff/dj.example.com/current/staticfiles/;",
	} {
		if !strings.Contains(stdin, want) {
			t.Errorf("python deploy inputs missing %q", want)
		}
	}
	if strings.Contains(all, `PHP="8.`) || strings.Contains(all, "-fpm reload") || strings.Contains(all, "Installing Node.js") {
		t.Error("a Python app deploy must not install PHP or Node.js")
	}
	bad := djangoSite
	bad.PythonVersion = "2.7"
	if err := (&Deployer{Exec: &recorder{}}).Deploy(context.Background(), &bytes.Buffer{}, bad, "", nil); err == nil {
		t.Error("an unsupported Python version was accepted")
	}
}

func TestPythonCommandsGetVenvAndEnv(t *testing.T) {
	rec := &recorder{}
	if err := (&Deployer{Exec: rec}).RunCommand(context.Background(), &bytes.Buffer{}, djangoSite, "python manage.py check"); err != nil {
		t.Fatal(err)
	}
	s := rec.scripts[0]
	for _, want := range []string{"UV_PYTHON=3.13", `export VIRTUAL_ENV="$PWD/.venv"`, "set -a; . ./.env; set +a", "python manage.py check"} {
		if !strings.Contains(s, want) {
			t.Errorf("command script missing %q:\n%s", want, s)
		}
	}
}
