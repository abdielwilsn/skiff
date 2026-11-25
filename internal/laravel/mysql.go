package laravel

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strings"

	"skiff/internal/executor"
	"skiff/internal/store"
)

// MySQL is installed on demand (not during provisioning), bound to localhost,
// and tuned down on small servers. Skiff talks to it as root over the unix
// socket (Ubuntu's default auth_socket), so no root password exists or is stored.

// DBSpec is what Skiff needs to create or connect to a database.
type DBSpec struct {
	Engine   string // store.EngineMySQL (default) or store.EnginePostgres
	Name     string
	User     string
	Password string
}

func SpecOf(d store.Database) DBSpec {
	return DBSpec{Engine: d.Engine, Name: d.Name, User: d.Username, Password: d.Password}
}

func (spec DBSpec) postgres() bool { return spec.Engine == store.EnginePostgres }

// label is the engine's display name.
func (spec DBSpec) label() string {
	if spec.postgres() {
		return "PostgreSQL"
	}
	return "MySQL"
}

// dbNameRe keeps names safe to splice into SQL identifiers and shell scripts.
// MySQL user names are limited to 32 characters.
var dbNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

var reservedDBNames = map[string]bool{
	"mysql": true, "sys": true, "information_schema": true, "performance_schema": true,
	"root": true, "debian_sys_maint": true, "mysql_sys": true, "skiff": true,
	"postgres": true, "template0": true, "template1": true, "public": true,
}

// ValidDBName reports whether name can be used for a database and its user,
// in MySQL and PostgreSQL (where "pg_" role names are reserved).
func ValidDBName(name string) bool {
	return dbNameRe.MatchString(name) && !reservedDBNames[name] && !strings.HasPrefix(name, "pg_")
}

// DBNameFor turns a domain into a database name: "app.example.com" -> "app_example_com".
func DBNameFor(domain string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(domain) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	name := strings.Trim(b.String(), "_")
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = "db_" + name
	}
	if len(name) > 28 { // leave room for a "_2" style suffix
		name = strings.TrimRight(name[:28], "_")
	}
	if reservedDBNames[name] {
		name += "_db"
	}
	if strings.HasPrefix(name, "pg_") {
		name = strings.TrimRight(("db_" + name)[:min(len(name)+3, 28)], "_")
	}
	return name
}

// NewPassword returns a random alphanumeric password (no characters that need
// quoting in SQL, shell or .env files).
func NewPassword() string {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 28)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			panic(err)
		}
		b[i] = alphabet[n.Int64()]
	}
	return string(b)
}

func (spec DBSpec) valid() error {
	if !ValidDBName(spec.Name) || !ValidDBName(spec.User) {
		return fmt.Errorf("invalid database or user name %q/%q", spec.Name, spec.User)
	}
	if len(spec.Password) < 16 {
		return fmt.Errorf("database password is too short")
	}
	for _, r := range spec.Password {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return fmt.Errorf("database password may only contain letters and digits")
		}
	}
	return nil
}

const installMySQLTemplate = pkgsInstalledFn + `
export DEBIAN_FRONTEND=noninteractive
if ! pkgs_installed mysql-server; then
    echo "==> Installing MySQL"
    dpkg --configure -a || true   # finish any interrupted install first
    apt_update
    apt_install mysql-server
else
    echo "MySQL is already installed"
fi

echo "==> Configuring MySQL"
MEM_MB=${SKIFF_MEM_MB:-$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)}
{
    echo "# Managed by Skiff."
    echo "[mysqld]"
    echo "bind-address = 127.0.0.1"
    echo "mysqlx = OFF"
    if [ "$MEM_MB" -lt 2048 ]; then
        BUFFER=128M
        if [ "$MEM_MB" -lt 1200 ]; then BUFFER=64M; fi
        echo "# Small server (${MEM_MB} MB RAM): keep MySQL's footprint low."
        echo "performance_schema = OFF"
        echo "innodb_buffer_pool_size = ${BUFFER}"
        echo "innodb_log_buffer_size = 8M"
        echo "max_connections = 50"
        echo "key_buffer_size = 8M"
        echo "tmp_table_size = 16M"
        echo "max_heap_table_size = 16M"
        echo "table_open_cache = 400"
        echo "thread_cache_size = 8"
    fi
} > /etc/mysql/mysql.conf.d/zz-skiff.cnf
cat /etc/mysql/mysql.conf.d/zz-skiff.cnf

service mysql restart
for i in $(seq 1 30); do
    mysqladmin --user=root ping >/dev/null 2>&1 && break
    sleep 1
done
mysql --user=root -N -e "SELECT CONCAT('MySQL ', VERSION(), ' is running')"
`

// InstallMySQL installs and configures MySQL. Safe to re-run.
func (d *Deployer) InstallMySQL(ctx context.Context, w io.Writer) error {
	return d.run(ctx, w, installMySQLTemplate, executor.Options{})
}

// mysqlHosts: Laravel connects to 127.0.0.1 over TCP; the CLI uses the socket
// ("localhost"). Creating the user for both avoids depending on name resolution.
var mysqlHosts = []string{"localhost", "127.0.0.1"}

// CreateDatabase creates the database and its user (idempotent; it also
// resets the user's password to spec.Password).
func (d *Deployer) CreateDatabase(ctx context.Context, w io.Writer, spec DBSpec) error {
	if err := spec.valid(); err != nil {
		return err
	}
	if spec.postgres() {
		return d.createPostgres(ctx, w, spec)
	}
	var sql strings.Builder
	fmt.Fprintf(&sql, "CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;\n", spec.Name)
	for _, h := range mysqlHosts {
		fmt.Fprintf(&sql, "CREATE USER IF NOT EXISTS '%[1]s'@'%[2]s' IDENTIFIED BY '%[3]s';\n", spec.User, h, spec.Password)
		fmt.Fprintf(&sql, "ALTER USER '%[1]s'@'%[2]s' IDENTIFIED BY '%[3]s';\n", spec.User, h, spec.Password)
		fmt.Fprintf(&sql, "GRANT ALL PRIVILEGES ON `%[1]s`.* TO '%[2]s'@'%[3]s';\n", spec.Name, spec.User, h)
	}
	sql.WriteString("FLUSH PRIVILEGES;\n")

	step(w, "Creating MySQL database %s (user %s)", spec.Name, spec.User)
	// SQL goes over stdin so the password never appears in a process list or the log.
	script := `command -v mysql >/dev/null 2>&1 || { echo "MySQL is not installed. Install it from the Databases page." >&2; exit 1; }
mysql --user=root
echo "OK"`
	if err := d.run(ctx, w, script, executor.Options{Stdin: strings.NewReader(sql.String())}); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	return nil
}

// DropDatabase deletes the database and its user. All data in it is lost.
func (d *Deployer) DropDatabase(ctx context.Context, w io.Writer, spec DBSpec) error {
	if !ValidDBName(spec.Name) || !ValidDBName(spec.User) {
		return fmt.Errorf("invalid database name")
	}
	if spec.postgres() {
		return d.dropPostgres(ctx, w, spec)
	}
	var sql strings.Builder
	fmt.Fprintf(&sql, "DROP DATABASE IF EXISTS `%s`;\n", spec.Name)
	for _, h := range mysqlHosts {
		fmt.Fprintf(&sql, "DROP USER IF EXISTS '%s'@'%s';\n", spec.User, h)
	}
	step(w, "Dropping MySQL database %s and user %s", spec.Name, spec.User)
	if err := d.run(ctx, w, "mysql --user=root\necho OK", executor.Options{Stdin: strings.NewReader(sql.String())}); err != nil {
		return fmt.Errorf("drop database: %w", err)
	}
	return nil
}

// setEnvFn defines set_env KEY VALUE, which sets a key in "$ENVF",
// uncommenting it or appending it as needed.
const setEnvFn = `set_env() {
    if grep -qE "^$1=" "$ENVF"; then
        sed -i "s|^$1=.*|$1=$2|" "$ENVF"
    elif grep -qE "^# *$1=" "$ENVF"; then
        sed -i "s|^# *$1=.*|$1=$2|" "$ENVF"
    else
        printf '%s=%s\n' "$1" "$2" >> "$ENVF"
    fi
}
mkdir -p "$(dirname "$ENVF")"
touch "$ENVF"
chmod 640 "$ENVF"
`

// configCache refreshes a Laravel app's config cache if it uses one.
const configCache = `if [ -f "$APP/bootstrap/cache/config.php" ]; then cd "$APP" && php artisan config:cache; fi`

// WriteDatabaseEnv points the site's .env at the database: Laravel's DB_*
// keys, or DATABASE_URL for other apps (Django with dj-database-url, Prisma,
// Drizzle, Rails-style libraries…). It refreshes Laravel's config cache.
func (d *Deployer) WriteDatabaseEnv(ctx context.Context, w io.Writer, site store.Site, spec DBSpec) error {
	if err := spec.valid(); err != nil {
		return err
	}
	conn, port, scheme := "mysql", "3306", "mysql"
	if spec.postgres() {
		conn, port, scheme = "pgsql", "5432", "postgres"
	}
	step(w, "Pointing %s's .env at %s database %s", site.Domain, spec.label(), spec.Name)
	var keys string
	if site.IsLaravel() {
		keys = fmt.Sprintf(`set_env DB_CONNECTION %[1]s
set_env DB_HOST 127.0.0.1
set_env DB_PORT %[2]s
set_env DB_DATABASE %[3]s
set_env DB_USERNAME %[4]s
set_env DB_PASSWORD "$SKIFF_DB_PASSWORD"
echo "DB_CONNECTION=%[1]s, DB_DATABASE=%[3]s, DB_USERNAME=%[4]s"
`, conn, port, spec.Name, spec.User)
	} else {
		keys = fmt.Sprintf(`set_env DATABASE_URL "%[1]s://%[3]s:$SKIFF_DB_PASSWORD@127.0.0.1:%[2]s/%[4]s"
echo "DATABASE_URL=%[1]s://%[3]s:…@127.0.0.1:%[2]s/%[4]s"
`, scheme, port, spec.User, spec.Name)
	}
	script := locate(site.Domain) + setEnvFn + keys + configCache
	// The password is passed in the environment, not the script, so it isn't logged.
	opt := executor.Options{User: SiteUser, Env: []string{"SKIFF_DB_PASSWORD=" + spec.Password}}
	if err := d.run(ctx, w, withPHP(site.PHPVersion, script), opt); err != nil {
		return fmt.Errorf("update .env: %w", err)
	}
	return nil
}

// Migrate runs the site's migrations, if it has been deployed.
func (d *Deployer) Migrate(ctx context.Context, w io.Writer, site store.Site) error {
	step(w, "Running migrations")
	script := locate(site.Domain) + `cd "$APP" 2>/dev/null || true
if [ -f artisan ] && [ -d vendor ]; then
    php artisan migrate --force
else
    echo "Site isn't fully deployed yet; migrations will run on the next deploy."
fi`
	return d.run(ctx, w, withPHP(site.PHPVersion, script), executor.Options{User: SiteUser})
}
