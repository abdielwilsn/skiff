package laravel

import (
	"context"
	"fmt"
	"io"
	"strings"

	"skiff/internal/executor"
)

// Each site picks a PHP version. A version is installed the first time a site
// needs it (FPM, CLI and the usual Laravel extensions, from ppa:ondrej/php when
// Ubuntu doesn't ship it), gets its own "skiff" FPM pool, and a shim directory
// so "php" and "composer" in that site's scripts run that version.

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

const ensurePHPTemplate = pkgsInstalledFn + `
PHP="{{PHP}}"
PHP_PKGS="php${PHP}-fpm php${PHP}-cli php${PHP}-mbstring php${PHP}-xml php${PHP}-curl php${PHP}-zip php${PHP}-bcmath php${PHP}-intl php${PHP}-sqlite3 php${PHP}-mysql php${PHP}-gd"
if pkgs_installed $PHP_PKGS; then
    echo "PHP ${PHP} is installed"
else
    echo "==> Installing PHP ${PHP}"
    export DEBIAN_FRONTEND=noninteractive
    dpkg --configure -a || true
    apt-get update -q
    if ! apt-cache show "php${PHP}-fpm" >/dev/null 2>&1; then
        echo "PHP ${PHP} isn't in Ubuntu's repositories; adding ppa:ondrej/php"
        apt-get install -y -q software-properties-common
        add-apt-repository -y ppa:ondrej/php
        apt-get update -q
    fi
    apt-get install -y -q $PHP_PKGS
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
