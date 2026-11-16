package laravel

import (
	"context"
	"fmt"
	"io"
	"strings"

	"skiff/internal/executor"
	"skiff/internal/shell"
)

// Node.js is installed per major version from the official nodejs.org builds
// (checksum-verified) into /opt/skiff/node/<major>, the first time a site with
// a package.json deploys. No apt repositories or install scripts are involved,
// and servers whose apps don't need Node never get it.

// DefaultNode is the Node.js major version new sites use.
const DefaultNode = "24"

// NodeVersions are the LTS majors a site can choose. "" means no Node.
var NodeVersions = []string{"22", "24", "26"}

func ValidNode(v string) bool {
	if v == "" {
		return true
	}
	for _, x := range NodeVersions {
		if x == v {
			return true
		}
	}
	return false
}

func nodeDir(major string) string { return "/opt/skiff/node/" + major }

// withNode puts the site's Node.js (node, npm, npx) first on PATH. If that
// version isn't installed (no package.json), the PATH entry is simply unused.
func withNode(major, script string) string {
	if major == "" || !ValidNode(major) {
		return script
	}
	return fmt.Sprintf("export PATH=%s/bin:$PATH\n", nodeDir(major)) + script
}

const ensureNodeTemplate = `
SITE={{SITE}}
MAJOR="{{MAJOR}}"
DIR="{{DIR}}"
if [ ! -f "$SITE/package.json" ]; then
    echo "No package.json; Node.js isn't needed"
    exit 0
fi
if [ -x "$DIR/bin/node" ]; then
    echo "Node.js $("$DIR/bin/node" -v) is installed"
    exit 0
fi

echo "==> Installing Node.js ${MAJOR} (official build)"
case "$(uname -m)" in
    x86_64) ARCH=x64 ;;
    aarch64) ARCH=arm64 ;;
    *) echo "Unsupported CPU architecture: $(uname -m)" >&2; exit 1 ;;
esac
if ! command -v xz >/dev/null 2>&1; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q xz-utils
fi
BASE="https://nodejs.org/dist/latest-v${MAJOR}.x"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
curl -fsSL "$BASE/SHASUMS256.txt" -o "$TMP/SHASUMS256.txt"
FILE="$(grep -oE "node-v[0-9.]+-linux-${ARCH}\.tar\.xz" "$TMP/SHASUMS256.txt" | head -n 1)"
[ -n "$FILE" ] || { echo "Couldn't find a Node.js ${MAJOR} build for linux-${ARCH}" >&2; exit 1; }
echo "Downloading $FILE"
curl -fsSL "$BASE/$FILE" -o "$TMP/$FILE"
(cd "$TMP" && grep " $FILE\$" SHASUMS256.txt | sha256sum -c -)
mkdir -p "$DIR.tmp"
# --no-same-owner + chown: the archive's file owner (uid 1001) may be a real
# user here, e.g. the site user. Node must be root-owned so a site can't
# replace binaries that Skiff later runs as root.
tar -xJf "$TMP/$FILE" -C "$DIR.tmp" --strip-components=1 --no-same-owner
chown -R root:root "$DIR.tmp"
chmod -R go-w "$DIR.tmp"
rm -rf "$DIR"
mv "$DIR.tmp" "$DIR"
echo "Installed Node.js $("$DIR/bin/node" -v), npm $(PATH="$DIR/bin:$PATH" npm -v)"
`

// EnsureNode installs the site's Node.js version if the site has a
// package.json and that version isn't installed yet.
func (d *Deployer) EnsureNode(ctx context.Context, w io.Writer, sitePath, major string) error {
	if major == "" {
		return nil
	}
	if !ValidNode(major) {
		return fmt.Errorf("unsupported Node.js version %q", major)
	}
	step(w, "Checking Node.js %s", major)
	script := strings.NewReplacer(
		"{{SITE}}", shell.Quote(sitePath),
		"{{MAJOR}}", major,
		"{{DIR}}", nodeDir(major),
	).Replace(ensureNodeTemplate)
	if err := d.run(ctx, w, script, executor.Options{}); err != nil {
		return fmt.Errorf("install Node.js %s: %w", major, err)
	}
	return nil
}
