// Package laravel provisions an Ubuntu server for Laravel and deploys sites to it.
package laravel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"skiff/internal/executor"
	"skiff/internal/shell"
	"skiff/internal/store"
)

const (
	SiteUser  = "skiff"       // unprivileged user every site runs as
	SitesRoot = "/home/skiff" // each site lives in SitesRoot/<domain>
)

func SitePath(domain string) string        { return SitesRoot + "/" + domain }
func NginxConfigPath(domain string) string { return "/etc/nginx/sites-available/skiff-" + domain }
func nginxEnabledPath(domain string) string {
	return "/etc/nginx/sites-enabled/skiff-" + domain
}
func fpmSocket(php string) string { return "/run/php/php" + php + "-fpm-" + SiteUser + ".sock" }

// DefaultDeployScript runs after the latest commit is checked out. Users can
// edit it per site, like Forge's deploy script.
const DefaultDeployScript = `# Runs as the "skiff" user inside the site directory, after Skiff has
# fetched the latest commit. Any failing command stops the deploy.
# php, composer, node and npm are the versions chosen for this site.
# Available: $SKIFF_SITE_PATH $SKIFF_BRANCH $SKIFF_DOMAIN $SKIFF_PHP $SKIFF_NODE

composer install --no-dev --no-interaction --prefer-dist --optimize-autoloader

# Drop the previous deploy's cached config and routes so everything below
# (including Vite plugins such as Wayfinder) sees this commit's routes.
# (Not optimize:clear: it also empties the app cache, which can live in a
# database table that doesn't exist yet on a first deploy.)
php artisan config:clear
php artisan route:clear

if ! grep -q '^APP_KEY=base64:' .env; then
    php artisan key:generate --force
fi

# Frontend assets (Vite etc.)
if [ -f package.json ]; then
    if [ -f package-lock.json ]; then npm ci; else npm install; fi
    npm run build
fi

php artisan migrate --force
php artisan optimize
`

// PreviousDefaultDeployScripts are earlier versions of DefaultDeployScript.
// Sites still using one unchanged are upgraded to the current default.
var PreviousDefaultDeployScripts = []string{`# Runs as the "skiff" user inside the site directory, after Skiff has
# fetched the latest commit. Any failing command stops the deploy.
# php, composer, node and npm are the versions chosen for this site.
# Available: $SKIFF_SITE_PATH $SKIFF_BRANCH $SKIFF_DOMAIN $SKIFF_PHP $SKIFF_NODE

composer install --no-dev --no-interaction --prefer-dist --optimize-autoloader

if ! grep -q '^APP_KEY=base64:' .env; then
    php artisan key:generate --force
fi

# Frontend assets (Vite etc.)
if [ -f package.json ]; then
    if [ -f package-lock.json ]; then npm ci; else npm install; fi
    npm run build
fi

php artisan migrate --force
php artisan optimize
`, `# Runs as the "skiff" user inside the site directory, after Skiff has
# fetched the latest commit. Any failing command stops the deploy.
# Available: $SKIFF_SITE_PATH $SKIFF_BRANCH $SKIFF_DOMAIN $SKIFF_PHP

composer install --no-dev --no-interaction --prefer-dist --optimize-autoloader

if ! grep -q '^APP_KEY=base64:' .env; then
    php artisan key:generate --force
fi

php artisan migrate --force
php artisan optimize
`}

type Deployer struct {
	Exec executor.Executor

	// ACMEServer overrides Let's Encrypt's production directory, e.g. its
	// staging URL. ACMEInsecure skips TLS verification of that server and is
	// only meant for local test servers like Pebble.
	ACMEServer   string
	ACMEInsecure bool
}

func step(w io.Writer, format string, a ...any) {
	fmt.Fprintf(w, "\n==> "+format+"\n", a...)
}

func (d *Deployer) run(ctx context.Context, w io.Writer, script string, opt executor.Options) error {
	return d.Exec.Run(ctx, "set -euo pipefail\n"+script, opt, w)
}

// ---- provisioning ----------------------------------------------------------

const provisionTemplate = `
export DEBIAN_FRONTEND=noninteractive
export COMPOSER_ALLOW_SUPERUSER=1

if ! command -v apt-get >/dev/null 2>&1; then
    echo "Skiff currently supports Ubuntu servers only." >&2
    exit 1
fi

echo "==> Updating package lists"
apt-get update -q

echo "==> Installing Nginx and base packages"
apt-get install -y -q ca-certificates curl git unzip nginx

echo "==> Creating the {{USER}} user that sites run as"
if ! id -u {{USER}} >/dev/null 2>&1; then
    useradd --create-home --shell /bin/bash {{USER}}
fi
chmod 750 {{SITES_ROOT}}
usermod -aG {{USER}} www-data   # lets Nginx read public/ without opening up the home dir

{{ENSURE_PHP}}
update-alternatives --set php "/usr/bin/php{{PHP}}" >/dev/null 2>&1 || true

echo "==> Checking swap"
if [ -z "$(swapon --show --noheadings 2>/dev/null)" ] && [ ! -f /swapfile ]; then
    MEM_MB=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)
    if [ "$MEM_MB" -lt 2048 ]; then
        echo "Low memory (${MEM_MB} MB) and no swap: adding a 1 GB swap file so composer doesn't run out of memory"
        if fallocate -l 1G /swapfile && chmod 600 /swapfile && mkswap /swapfile >/dev/null && swapon /swapfile; then
            echo '/swapfile none swap sw 0 0' >> /etc/fstab
        else
            echo "Could not enable swap (normal inside containers); continuing"
            swapoff /swapfile 2>/dev/null || true
            rm -f /swapfile
        fi
    else
        echo "${MEM_MB} MB RAM; no swap needed"
    fi
else
    echo "Swap already configured"
fi

echo "==> Installing Composer"
if ! command -v composer >/dev/null 2>&1; then
    EXPECTED="$(curl -fsSL https://composer.github.io/installer.sig)"
    curl -fsSL https://getcomposer.org/installer -o /tmp/composer-setup.php
    ACTUAL="$(php -r "echo hash_file('sha384', '/tmp/composer-setup.php');")"
    if [ "$EXPECTED" != "$ACTUAL" ]; then
        rm -f /tmp/composer-setup.php
        echo "Composer installer checksum mismatch" >&2
        exit 1
    fi
    php /tmp/composer-setup.php --quiet --install-dir=/usr/local/bin --filename=composer
    rm -f /tmp/composer-setup.php
fi
composer --version

echo "==> Configuring Nginx"
rm -f /etc/nginx/sites-enabled/default
sed -i 's/# server_names_hash_bucket_size 64;/server_names_hash_bucket_size 64;/' /etc/nginx/nginx.conf
nginx -t
service nginx restart

echo "==> Server is ready"
php -v | head -n 1
nginx -v
`

func provisionScript(php string) string {
	return strings.NewReplacer(
		"{{ENSURE_PHP}}", ensurePHPScript(php),
		"{{PHP}}", php,
		"{{USER}}", SiteUser,
		"{{SITES_ROOT}}", SitesRoot,
	).Replace(provisionTemplate)
}

// Provision installs and configures everything Laravel sites need, with the
// default PHP version. It is idempotent, so it is safe to run again.
func (d *Deployer) Provision(ctx context.Context, w io.Writer) error {
	return d.run(ctx, w, provisionScript(DefaultPHP), executor.Options{})
}

// ---- sites -----------------------------------------------------------------

const nginxTemplate = `# Managed by Skiff. Created on the first deploy; your edits are kept afterwards.
server {
    listen 80;
    listen [::]:80;
    server_name {{DOMAIN}};
    root {{ROOT}};

    add_header X-Frame-Options "SAMEORIGIN";
    add_header X-Content-Type-Options "nosniff";

    index index.php;
    charset utf-8;
    client_max_body_size 64m;

    location / {
        try_files $uri $uri/ /index.php?$query_string;
    }

    location = /favicon.ico { access_log off; log_not_found off; }
    location = /robots.txt  { access_log off; log_not_found off; }

    error_page 404 /index.php;

    location ~ ^/index\.php(/|$) {
        fastcgi_pass unix:{{SOCKET}};
        fastcgi_param SCRIPT_FILENAME $realpath_root$fastcgi_script_name;
        include fastcgi_params;
        fastcgi_hide_header X-Powered-By;
        # Laravel can send large headers (Vite preload Link headers, cookies);
        # Nginx's 4-8k default turns those into 502 "too big header".
        fastcgi_buffer_size 64k;
        fastcgi_buffers 16 32k;
    }

    location ~ /\.(?!well-known).* {
        deny all;
    }
}
`

const nodeNginxTemplate = `# Managed by Skiff. Created on the first deploy; your edits are kept afterwards.
server {
    listen 80;
    listen [::]:80;
    server_name {{DOMAIN}};

    client_max_body_size 64m;

    location / {
        proxy_pass http://127.0.0.1:{{PORT}};
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        # WebSockets / server-sent events
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $http_connection;
        proxy_read_timeout 120s;
        proxy_buffer_size 64k;
        proxy_buffers 16 32k;
    }
}
`

const staticNginxTemplate = `# Managed by Skiff. Created on the first deploy; your edits are kept afterwards.
server {
    listen 80;
    listen [::]:80;
    server_name {{DOMAIN}};
    root {{ROOT}};
    index index.html;
    charset utf-8;

    add_header X-Frame-Options "SAMEORIGIN";
    add_header X-Content-Type-Options "nosniff";

    location / {
        # Single-page apps: unknown paths fall back to index.html
        try_files $uri $uri/ $uri.html /index.html;
    }

    # Fingerprinted build assets can be cached for a long time
    location ~* \.(?:js|mjs|css|woff2?|ttf|otf|eot|svg|png|jpe?g|gif|webp|avif|ico)$ {
        try_files $uri =404;
        expires 30d;
        access_log off;
    }

    location ~ /\.(?!well-known).* {
        deny all;
    }
}
`

// staticRoot is the folder Nginx serves for a static site.
func staticRoot(site store.Site) string {
	out := strings.Trim(site.OutputDir, "/")
	if out == "" || out == "." {
		return SitePath(site.Domain) + "/current"
	}
	return SitePath(site.Domain) + "/current/" + out
}

func NginxConfig(site store.Site) string {
	switch {
	case site.IsNode():
		return strings.NewReplacer("{{DOMAIN}}", site.Domain, "{{PORT}}", fmt.Sprint(site.Port)).Replace(nodeNginxTemplate)
	case site.IsStatic():
		return strings.NewReplacer("{{DOMAIN}}", site.Domain, "{{ROOT}}", staticRoot(site)).Replace(staticNginxTemplate)
	}
	return strings.NewReplacer(
		"{{DOMAIN}}", site.Domain,
		"{{ROOT}}", SitePath(site.Domain)+"/current/public",
		"{{SOCKET}}", fpmSocket(site.PHPVersion),
	).Replace(nginxTemplate)
}

// Delete removes the site's Nginx config and files.
func (d *Deployer) Delete(ctx context.Context, w io.Writer, site store.Site) error {
	dir := SitePath(site.Domain)
	if site.Domain == "" || strings.ContainsAny(site.Domain, "/ ") || !strings.HasPrefix(dir, SitesRoot+"/") {
		return errors.New("refusing to delete: unexpected site path")
	}
	q := shell.Quote
	step(w, "Removing Nginx config")
	ng := fmt.Sprintf(`rm -f %s %s
if command -v nginx >/dev/null 2>&1; then nginx -t && service nginx reload; fi`,
		q(nginxEnabledPath(site.Domain)), q(NginxConfigPath(site.Domain)))
	if err := d.run(ctx, w, ng, executor.Options{}); err != nil {
		return err
	}
	step(w, "Stopping background processes")
	if err := d.RemoveProcesses(ctx, w, site.Domain); err != nil {
		return err
	}
	if site.HTTPS {
		step(w, "Deleting the certificate")
		if err := d.run(ctx, w, d.deleteCertScript(site.Domain), executor.Options{}); err != nil {
			return err
		}
	}
	step(w, "Removing %s", dir)
	return d.run(ctx, w, "rm -rf --one-file-system -- "+q(dir), executor.Options{})
}

// ReadEnv returns the site's .env file ("" if it doesn't exist yet).
func (d *Deployer) ReadEnv(ctx context.Context, site store.Site) (string, error) {
	var out bytes.Buffer
	script := locate(site.Domain) + `cat -- "$ENVF" 2>/dev/null || true`
	err := d.Exec.Run(ctx, script, executor.Options{User: SiteUser}, &out)
	return out.String(), err
}

// WriteEnv atomically replaces the site's .env and refreshes Laravel's config
// cache if the app uses one.
func (d *Deployer) WriteEnv(ctx context.Context, site store.Site, content string) error {
	script := locate(site.Domain) + `test -d "$SITE" || { echo "The site has not been deployed yet." >&2; exit 1; }
tmp=$(mktemp "$(dirname "$ENVF")/.env.skiff.XXXXXX")
cat > "$tmp"
chmod 640 "$tmp"
mv "$tmp" "$ENVF"
if [ -f "$APP/bootstrap/cache/config.php" ]; then
    cd "$APP" && php artisan config:cache
fi`
	var out bytes.Buffer
	err := d.run(ctx, &out, withPHP(site.PHPVersion, script), executor.Options{User: SiteUser, Stdin: strings.NewReader(content)})
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}
