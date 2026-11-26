package laravel

import (
	"context"
	"fmt"
	"io"
	"strings"

	"skiff/internal/executor"
)

// Each site picks a PHP version. A version is installed the first time a site
// needs it (FPM, CLI and the usual Laravel extensions, from packages.sury.org
// when Ubuntu doesn't ship it), gets its own "skiff" FPM pool, and a shim
// directory so "php" and "composer" in that site's scripts run that version.

// DefaultPHP is what provisioning installs and new sites use. Laravel 13's
// dependencies (Symfony 8) need PHP 8.4.
const DefaultPHP = "8.4"

// PHPVersions are the versions a site can choose.
var PHPVersions = []string{"8.2", "8.3", "8.4", "8.5"}

func ValidPHP(v string) bool {
	for _, x := range PHPVersions {
		if x == v {
			return true
		}
	}
	return false
}

// phpShimDir holds a "php" symlink to /usr/bin/phpX.Y. Putting it first on
// PATH makes "php artisan" and composer's "#!/usr/bin/env php" use that version.
func phpShimDir(php string) string { return "/opt/skiff/php/" + php }

// withPHP prefixes a script so php and composer resolve to the given version.
func withPHP(php, script string) string {
	if !ValidPHP(php) {
		return script
	}
	return fmt.Sprintf("export PATH=%s:$PATH\n", phpShimDir(php)) + script
}

// phpRepoFns sets up Ondřej Surý's PHP repository, packages.sury.org/php, which
// carries every supported PHP version for current Ubuntu releases. It replaced
// ppa:ondrej/php, which is frozen and has no packages for Ubuntu 26.04;
// servers set up with the PPA are moved to it (installed packages stay and
// get their updates from the new repository).
const phpRepoFns = `sury_configured() {
    grep -rqs 'packages.sury.org/php' /etc/apt/sources.list /etc/apt/sources.list.d/
}
old_ppa_files() {
    ls /etc/apt/sources.list.d/ondrej-ubuntu-php-* 2>/dev/null | grep -v '\.save$' || true
}
# (grep without -q reads all its input: with -q it can exit early, and under
# pipefail the writer's broken pipe would make a match look like a failure.)
php_available() {
    apt-cache show "php$1-fpm" 2>/dev/null | grep '^Version:' >/dev/null
}
# add_sury returns non-zero on any failure. (It's called from "if", where
# set -e doesn't apply, so every step checks for itself.)
add_sury() {
    local code tmp
    code="$(. /etc/os-release && echo "${VERSION_CODENAME:-${UBUNTU_CODENAME:-}}")"
    [ -n "$code" ] || { echo "Can't tell which Ubuntu release this is." >&2; return 1; }
    apt_install ca-certificates curl || return 1
    if ! curl -fsS -o /dev/null "https://packages.sury.org/php/dists/${code}/InRelease"; then
        echo "packages.sury.org has no PHP packages for Ubuntu ${code}." >&2
        return 1
    fi
    tmp="$(mktemp -d)" || return 1
    curl -fsSL -o "$tmp/keyring.deb" https://packages.sury.org/debsuryorg-archive-keyring.deb \
        && dpkg -i "$tmp/keyring.deb" >/dev/null \
        && [ -s /usr/share/keyrings/debsuryorg-archive-keyring.gpg ] \
        || { rm -rf "$tmp"; echo "Couldn't install the packages.sury.org signing key." >&2; return 1; }
    rm -rf "$tmp"
    echo "deb [signed-by=/usr/share/keyrings/debsuryorg-archive-keyring.gpg] https://packages.sury.org/php/ ${code} main" \
        > /etc/apt/sources.list.d/skiff-sury-php.list || return 1
}
`

const ensurePHPTemplate = pkgsInstalledFn + phpRepoFns + `
PHP="{{PHP}}"
PHP_PKGS="php${PHP}-fpm php${PHP}-cli php${PHP}-mbstring php${PHP}-xml php${PHP}-curl php${PHP}-zip php${PHP}-bcmath php${PHP}-intl php${PHP}-sqlite3 php${PHP}-mysql php${PHP}-pgsql php${PHP}-gd"
export DEBIAN_FRONTEND=noninteractive

OLD_PPA="$(old_ppa_files)"
if [ -n "$OLD_PPA" ] && ! sury_configured; then
    echo "==> Moving PHP updates from ppa:ondrej/php (frozen) to packages.sury.org"
    if add_sury; then
        rm -f $OLD_PPA
        apt_update
    else
        echo "Keeping ppa:ondrej/php for now."
    fi
fi

if pkgs_installed $PHP_PKGS; then
    echo "PHP ${PHP} is installed"
else
    echo "==> Installing PHP ${PHP}"
    dpkg --configure -a || true
    apt_update
    if ! php_available "$PHP"; then
        if sury_configured; then
            echo "PHP ${PHP} isn't available from Ubuntu or packages.sury.org for this release." >&2
            exit 1
        fi
        echo "PHP ${PHP} isn't in Ubuntu's repositories; adding packages.sury.org/php"
        add_sury || exit 1
        apt_update
    fi
    apt_install $PHP_PKGS
fi

POOL="/etc/php/${PHP}/fpm/pool.d/{{USER}}.conf"
if [ ! -f "$POOL" ]; then
    echo "==> Configuring PHP ${PHP}-FPM"
    cat > "$POOL" <<'EOF'
; Managed by Skiff
[{{USER}}]
user = {{USER}}
group = {{USER}}
listen = {{SOCKET}}
listen.owner = www-data
listen.group = www-data
listen.mode = 0660
; ondemand: no idle PHP workers, which matters on tiny servers
pm = ondemand
pm.max_children = 5
pm.process_idle_timeout = 10s
pm.max_requests = 500
EOF
    sed -i 's/^pm = dynamic/pm = ondemand/' "/etc/php/${PHP}/fpm/pool.d/www.conf"
    service "php${PHP}-fpm" restart
fi

mkdir -p "{{SHIM}}"
ln -sfn "/usr/bin/php${PHP}" "{{SHIM}}/php"
`

func ensurePHPScript(php string) string {
	return strings.NewReplacer(
		"{{PHP}}", php,
		"{{USER}}", SiteUser,
		"{{SOCKET}}", fpmSocket(php),
		"{{SHIM}}", phpShimDir(php),
	).Replace(ensurePHPTemplate)
}

// EnsurePHP installs and configures a PHP version if it isn't already.
func (d *Deployer) EnsurePHP(ctx context.Context, w io.Writer, php string) error {
	if !ValidPHP(php) {
		return fmt.Errorf("unsupported PHP version %q", php)
	}
	step(w, "Checking PHP %s", php)
	if err := d.run(ctx, w, ensurePHPScript(php), executor.Options{}); err != nil {
		return fmt.Errorf("install PHP %s: %w", php, err)
	}
	return nil
}
