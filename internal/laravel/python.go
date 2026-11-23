package laravel

import (
	"context"
	"fmt"
	"io"
	"strings"

	"skiff/internal/executor"
	"skiff/internal/store"
)

// Python apps use uv (https://docs.astral.sh/uv/): a single static binary that
// installs Python versions, creates virtualenvs and installs packages, fast.
// uv is downloaded from its GitHub release (checksum-verified) into
// /opt/skiff/uv the first time a Python site deploys, and each Python version
// a site picks is installed by uv into /opt/skiff/python. Both are root-owned;
// sites only read them. Every release gets its own .venv, so a rollback runs
// with exactly the packages that release was built with.

// DefaultPython is the Python version new sites use.
const DefaultPython = "3.13"

// PythonVersions are the versions a site can choose.
var PythonVersions = []string{"3.11", "3.12", "3.13", "3.14"}

func ValidPython(v string) bool {
	for _, x := range PythonVersions {
		if x == v {
			return true
		}
	}
	return false
}

const (
	uvDir     = "/opt/skiff/uv"
	pythonDir = "/opt/skiff/python"
)

// pythonEnv is the environment every Python step runs with: uv on PATH, using
// only the Pythons Skiff installed (never downloading as the site user).
func pythonEnv(version string) string {
	return fmt.Sprintf(`export PATH=%s:$PATH
export UV_PYTHON_INSTALL_DIR=%s UV_PYTHON_PREFERENCE=only-managed UV_PYTHON_DOWNLOADS=never
export UV_PYTHON=%s UV_NO_PROGRESS=1
`, uvDir, pythonDir, version)
}

// withPython prepares a script for a Python site: uv and the site's Python,
// and, when the script runs in a release that has one, its .venv first on PATH.
func withPython(site store.Site, script string) string {
	if !site.IsPython() || !ValidPython(site.PythonVersion) {
		return script
	}
	return pythonEnv(site.PythonVersion) + `if [ -d .venv ]; then export VIRTUAL_ENV="$PWD/.venv" PATH="$PWD/.venv/bin:$PATH"; fi
` + script
}

const ensurePythonTemplate = `
UV_DIR="{{UV_DIR}}"
VERSION="{{VERSION}}"
if [ ! -x "$UV_DIR/uv" ]; then
    echo "==> Installing uv (Python package and version manager)"
    case "$(uname -m)" in
        x86_64) TARGET=x86_64-unknown-linux-gnu ;;
        aarch64) TARGET=aarch64-unknown-linux-gnu ;;
        *) echo "Unsupported CPU architecture: $(uname -m)" >&2; exit 1 ;;
    esac
    FILE="uv-$TARGET.tar.gz"
    BASE="https://github.com/astral-sh/uv/releases/latest/download"
    TMP="$(mktemp -d)"
    trap 'rm -rf "$TMP"' EXIT
    curl -fsSL --retry 3 --retry-all-errors "$BASE/$FILE" -o "$TMP/$FILE"
    curl -fsSL --retry 3 --retry-all-errors "$BASE/$FILE.sha256" -o "$TMP/$FILE.sha256"
    WANT="$(awk '{print $1; exit}' "$TMP/$FILE.sha256")"
    GOT="$(sha256sum "$TMP/$FILE" | awk '{print $1}')"
    [ -n "$WANT" ] && [ "$WANT" = "$GOT" ] || { echo "uv checksum mismatch" >&2; exit 1; }
    mkdir -p "$UV_DIR.tmp"
    tar -xzf "$TMP/$FILE" -C "$UV_DIR.tmp" --strip-components=1 --no-same-owner
    chown -R root:root "$UV_DIR.tmp"
    chmod -R go-w "$UV_DIR.tmp"
    rm -rf "$UV_DIR"
    mv "$UV_DIR.tmp" "$UV_DIR"
fi
"$UV_DIR/uv" --version

{{ENV}}
export UV_PYTHON_DOWNLOADS=automatic
mkdir -p "$UV_PYTHON_INSTALL_DIR"
if uv python find "$VERSION" >/dev/null 2>&1; then
    echo "Python $VERSION is installed: $(uv python find "$VERSION")"
else
    echo "==> Installing Python $VERSION"
    uv python install "$VERSION"
fi
chmod -R go+rX "$UV_PYTHON_INSTALL_DIR"
`

// EnsurePython installs uv and the site's Python version if needed.
func (d *Deployer) EnsurePython(ctx context.Context, w io.Writer, version string) error {
	if !ValidPython(version) {
		return fmt.Errorf("unsupported Python version %q", version)
	}
	step(w, "Checking Python %s", version)
	script := strings.NewReplacer(
		"{{UV_DIR}}", uvDir,
		"{{VERSION}}", version,
		"{{ENV}}", pythonEnv(version),
	).Replace(ensurePythonTemplate)
	if err := d.run(ctx, w, script, executor.Options{}); err != nil {
		return fmt.Errorf("install Python %s: %w", version, err)
	}
	return nil
}

// DefaultPythonDeployScript builds a release's virtualenv and runs Django's
// release steps when the project is a Django app. {{SERVER}} is replaced with
// the app server a preset needs (gunicorn or uvicorn), if any.
const DefaultPythonDeployScript = `# Runs as the "skiff" user inside the new release, with uv and this site's
# Python first on PATH. Any failing command stops the deploy.
# Available: $SKIFF_SITE_PATH $SKIFF_BRANCH $SKIFF_DOMAIN $SKIFF_PYTHON

# Settings from .env (Django and friends read os.environ)
if [ -f .env ]; then set -a; . ./.env; set +a; fi

# A fresh virtualenv per release, so rollbacks keep their own packages.
uv venv --quiet .venv
export VIRTUAL_ENV="$PWD/.venv" PATH="$PWD/.venv/bin:$PATH"

if [ -f uv.lock ]; then
    uv sync --frozen --no-dev
elif [ -f requirements.txt ]; then
    uv pip install -r requirements.txt
elif [ -f pyproject.toml ]; then
    uv pip install .
fi
{{SERVER}}
# Django: static files and migrations
if [ -f manage.py ]; then
    if python manage.py shell -c "from django.conf import settings; import sys; sys.exit(0 if settings.STATIC_ROOT else 1)" 2>/dev/null; then
        python manage.py collectstatic --noinput
    else
        echo "STATIC_ROOT isn't set; skipping collectstatic"
    fi
    python manage.py migrate --noinput
fi
`

// pythonDeployScript returns the default deploy script for a Python preset.
func pythonDeployScript(server string) string {
	line := ""
	if server != "" {
		line = "\n# The app server the start command uses (a no-op if it's already installed)\nuv pip install " + server + "\n"
	}
	return strings.Replace(DefaultPythonDeployScript, "{{SERVER}}", line, 1)
}

// detectWSGI is part of a Python app's start script: it sets WSGI_MODULE to
// the Django project package (the folder with wsgi.py) unless .env sets it.
const detectWSGI = `if [ -z "${WSGI_MODULE:-}" ]; then
    for f in */wsgi.py; do [ -f "$f" ] && { WSGI_MODULE="$(dirname "$f").wsgi"; break; }; done
fi
export WSGI_MODULE="${WSGI_MODULE:-wsgi}"
`
