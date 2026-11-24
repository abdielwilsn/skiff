package laravel

import (
	"context"
	"fmt"
	"io"
	"regexp"

	"skiff/internal/executor"
	"skiff/internal/store"
)

// Redis is installed on demand from Ubuntu's repositories, listens on
// localhost only and requires a password, which Skiff generates and writes
// into the .env of each site that uses it. Sites share the server's Redis;
// Laravel sites each get their own key prefix so their caches, sessions and
// queues never mix (staging's included).

const installRedisTemplate = pkgsInstalledFn + `
export DEBIAN_FRONTEND=noninteractive
if ! pkgs_installed redis-server; then
    echo "==> Installing Redis"
    dpkg --configure -a || true
    apt-get update -q
    apt_install redis-server
else
    echo "Redis is already installed"
fi

echo "==> Configuring Redis"
CONF=/etc/redis/redis.conf
SK=/etc/redis/skiff.conf
umask 027
cat > "$SK" <<EOF
# Managed by Skiff: localhost only, password required.
bind 127.0.0.1 -::1
protected-mode yes
port 6379
requirepass $SKIFF_REDIS_PASSWORD
EOF
chown redis:redis "$SK"
chmod 640 "$SK"
grep -qxF "include $SK" "$CONF" || printf '\ninclude %s\n' "$SK" >> "$CONF"
echo "Wrote $SK (bind 127.0.0.1, password required)"
systemctl enable redis-server >/dev/null 2>&1 || true
systemctl restart redis-server
export REDISCLI_AUTH="$SKIFF_REDIS_PASSWORD"
for i in $(seq 1 30); do
    [ "$(redis-cli ping 2>/dev/null)" = PONG ] && break
    sleep 1
done
echo "Redis $(redis-cli info server | sed -n 's/^redis_version:\([^\r]*\).*/\1/p') is running: $(redis-cli ping)"
`

var redisPasswordRe = regexp.MustCompile(`^[A-Za-z0-9]{16,64}$`)

// InstallRedis installs Redis and sets its password. Safe to re-run; running
// it again with a new password changes it.
func (d *Deployer) InstallRedis(ctx context.Context, w io.Writer, password string) error {
	if !redisPasswordRe.MatchString(password) {
		return fmt.Errorf("invalid Redis password")
	}
	return d.run(ctx, w, installRedisTemplate, executor.Options{Env: []string{"SKIFF_REDIS_PASSWORD=" + password}})
}

// RedisPrefix is the key prefix a Laravel site uses, e.g. "app_example_com_".
func RedisPrefix(domain string) string { return DBNameFor(domain) + "_" }

// WriteRedisEnv points the site's .env at the server's Redis. Laravel sites
// get REDIS_* keys and their own REDIS_PREFIX; with drivers, the cache,
// sessions and queue switch to Redis too. Other apps get REDIS_URL.
func (d *Deployer) WriteRedisEnv(ctx context.Context, w io.Writer, site store.Site, password string, drivers bool) error {
	if !redisPasswordRe.MatchString(password) {
		return fmt.Errorf("invalid Redis password")
	}
	step(w, "Pointing %s's .env at Redis", site.Domain)
	var keys string
	if site.IsLaravel() {
		keys = fmt.Sprintf(`set_env REDIS_CLIENT phpredis
set_env REDIS_HOST 127.0.0.1
set_env REDIS_PORT 6379
set_env REDIS_PASSWORD "$SKIFF_REDIS_PASSWORD"
set_env REDIS_PREFIX %[1]s
echo "REDIS_HOST=127.0.0.1, REDIS_PORT=6379, REDIS_PREFIX=%[1]s"
`, RedisPrefix(site.Domain))
		if drivers {
			keys += `set_env CACHE_STORE redis
if grep -qE '^CACHE_DRIVER=' "$ENVF"; then set_env CACHE_DRIVER redis; fi
set_env SESSION_DRIVER redis
set_env QUEUE_CONNECTION redis
echo "CACHE_STORE=redis, SESSION_DRIVER=redis, QUEUE_CONNECTION=redis"
`
		}
	} else {
		// "default" is Redis's built-in user; an empty user name in the URL
		// makes some clients (redis-cli among them) log in as a user named "".
		keys = `set_env REDIS_URL "redis://default:$SKIFF_REDIS_PASSWORD@127.0.0.1:6379/0"
echo "REDIS_URL=redis://default:…@127.0.0.1:6379/0"
`
	}
	script := locate(site.Domain) + setEnvFn + keys + configCache
	opt := executor.Options{User: SiteUser, Env: []string{"SKIFF_REDIS_PASSWORD=" + password}}
	if err := d.run(ctx, w, withPHP(site.PHPVersion, script), opt); err != nil {
		return fmt.Errorf("update .env: %w", err)
	}
	return nil
}

// EnsurePHPRedis installs the phpredis extension for a PHP version if it's
// missing, so Laravel's default Redis client works.
func (d *Deployer) EnsurePHPRedis(ctx context.Context, w io.Writer, php string) error {
	if !ValidPHP(php) {
		return fmt.Errorf("unsupported PHP version %q", php)
	}
	script := pkgsInstalledFn + fmt.Sprintf(`PHP=%s
if "php$PHP" -m 2>/dev/null | grep -qx redis; then echo "PHP $PHP has the Redis extension"; exit 0; fi
echo "==> Installing the Redis extension for PHP $PHP"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
if apt-cache show "php$PHP-redis" 2>/dev/null | grep -q '^Version:'; then apt_install "php$PHP-redis"; else apt_install php-redis; fi
service "php$PHP-fpm" reload || service "php$PHP-fpm" restart
"php$PHP" -m | grep -qx redis && echo "Installed"`, php)
	if err := d.run(ctx, w, script, executor.Options{}); err != nil {
		return fmt.Errorf("install the PHP Redis extension: %w", err)
	}
	return nil
}
