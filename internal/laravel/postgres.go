package laravel

import (
	"context"
	"fmt"
	"io"
	"strings"

	"skiff/internal/executor"
	"skiff/internal/shell"
)

// PostgreSQL is installed on demand from Ubuntu's repositories, listens on
// localhost only, and is tuned down on small servers. Skiff talks to it as
// the "postgres" system user over the unix socket (peer authentication), so
// no superuser password exists or is stored. Each database is owned by its
// own login role, and other roles can't connect to it.

const installPostgresTemplate = pkgsInstalledFn + `
export DEBIAN_FRONTEND=noninteractive
if ! pkgs_installed postgresql; then
    echo "==> Installing PostgreSQL"
    dpkg --configure -a || true
    apt-get update -q
    apt_install postgresql
else
    echo "PostgreSQL is already installed"
fi

echo "==> Configuring PostgreSQL"
VER="$(ls /etc/postgresql | sort -V | tail -n 1)"
[ -n "$VER" ] || { echo "PostgreSQL's configuration wasn't found." >&2; exit 1; }
MEM_MB=${SKIFF_MEM_MB:-$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)}
CONF="/etc/postgresql/$VER/main/conf.d/zz-skiff.conf"
mkdir -p "$(dirname "$CONF")"
{
    echo "# Managed by Skiff."
    echo "listen_addresses = 'localhost'"
    if [ "$MEM_MB" -lt 2048 ]; then
        SB=128MB
        if [ "$MEM_MB" -lt 1200 ]; then SB=64MB; fi
        echo "# Small server (${MEM_MB} MB RAM): keep PostgreSQL's footprint low."
        echo "shared_buffers = $SB"
        echo "max_connections = 50"
        echo "work_mem = 4MB"
        echo "maintenance_work_mem = 32MB"
        echo "effective_cache_size = 256MB"
    fi
} > "$CONF"
cat "$CONF"
systemctl enable --now postgresql >/dev/null 2>&1 || true
systemctl restart postgresql
for i in $(seq 1 30); do
    runuser -u postgres -- psql -qtAc 'SELECT 1' >/dev/null 2>&1 && break
    sleep 1
done
runuser -u postgres -- psql -qtAc "SELECT 'PostgreSQL ' || current_setting('server_version') || ' is running'"
`

// InstallPostgres installs and configures PostgreSQL. Safe to re-run.
func (d *Deployer) InstallPostgres(ctx context.Context, w io.Writer) error {
	return d.run(ctx, w, installPostgresTemplate, executor.Options{})
}

// psql runs SQL from stdin as the postgres superuser. Errors stop it.
const psql = `command -v psql >/dev/null 2>&1 || { echo "PostgreSQL is not installed. Install it from the Databases page." >&2; exit 1; }
cd /tmp
runuser -u postgres -- psql -v ON_ERROR_STOP=1 -q -d postgres
echo "OK"`

func (d *Deployer) createPostgres(ctx context.Context, w io.Writer, spec DBSpec) error {
	// Names are validated (lowercase letters, digits, underscores) and the
	// password is alphanumeric, so they are safe inside quotes here.
	sql := fmt.Sprintf(`SELECT format('CREATE ROLE %%I LOGIN', '%[2]s') WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%[2]s')\gexec
ALTER ROLE "%[2]s" WITH LOGIN PASSWORD '%[3]s';
SELECT format('CREATE DATABASE %%I OWNER %%I ENCODING ''UTF8'' TEMPLATE template0', '%[1]s', '%[2]s') WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '%[1]s')\gexec
ALTER DATABASE "%[1]s" OWNER TO "%[2]s";
REVOKE ALL ON DATABASE "%[1]s" FROM PUBLIC;
GRANT ALL ON DATABASE "%[1]s" TO "%[2]s";
`, spec.Name, spec.User, spec.Password)
	step(w, "Creating PostgreSQL database %s (user %s)", spec.Name, spec.User)
	// SQL goes over stdin so the password never appears in a process list or the log.
	if err := d.run(ctx, w, psql, executor.Options{Stdin: strings.NewReader(sql)}); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	return nil
}

func (d *Deployer) dropPostgres(ctx context.Context, w io.Writer, spec DBSpec) error {
	sql := fmt.Sprintf("DROP DATABASE IF EXISTS \"%s\" WITH (FORCE);\nDROP ROLE IF EXISTS \"%s\";\n", spec.Name, spec.User)
	step(w, "Dropping PostgreSQL database %s and user %s", spec.Name, spec.User)
	if err := d.run(ctx, w, psql, executor.Options{Stdin: strings.NewReader(sql)}); err != nil {
		return fmt.Errorf("drop database: %w", err)
	}
	return nil
}

// BackupPostgres dumps a PostgreSQL database to file (plain SQL, without
// ownership, so it restores into any role) and returns its size.
func (d *Deployer) BackupPostgres(ctx context.Context, w io.Writer, dbName, file string) (int64, error) {
	if !ValidDBName(dbName) || !ValidBackupFile(file) {
		return 0, fmt.Errorf("invalid backup target")
	}
	step(w, "Backing up PostgreSQL database %s", dbName)
	script := fmt.Sprintf(backupPrelude, shell.Quote(file), BackupRoot) + fmt.Sprintf(`command -v pg_dump >/dev/null 2>&1 || { echo "PostgreSQL isn't installed." >&2; exit 1; }
cd /tmp
runuser -u postgres -- pg_dump --no-owner --no-privileges %s | gzip -6 > "$TMP"
`, dbName) + backupFinish
	return d.runSized(ctx, w, script, executor.Options{})
}

// RestorePostgres replaces a PostgreSQL database's contents with a backup.
// The database is recreated (disconnecting the app) and the dump is loaded as
// its owner, so the app keeps owning every table.
func (d *Deployer) RestorePostgres(ctx context.Context, w io.Writer, dbName, owner, file string) error {
	if !ValidDBName(dbName) || !ValidDBName(owner) || !ValidBackupFile(file) || !strings.HasSuffix(file, ".sql.gz") {
		return fmt.Errorf("invalid restore")
	}
	step(w, "Restoring PostgreSQL database %s from %s", dbName, file)
	script := fmt.Sprintf(`FILE=%[1]s
[ -f "$FILE" ] || { echo "The backup file is missing." >&2; exit 1; }
gzip -t "$FILE"
cd /tmp
pg() { runuser -u postgres -- psql -v ON_ERROR_STOP=1 -q "$@"; }
pg -d postgres -c 'DROP DATABASE IF EXISTS "%[2]s" WITH (FORCE)'
pg -d postgres -c "CREATE DATABASE \"%[2]s\" OWNER \"%[3]s\" ENCODING 'UTF8' TEMPLATE template0"
pg -d postgres -c 'REVOKE ALL ON DATABASE "%[2]s" FROM PUBLIC' -c 'GRANT ALL ON DATABASE "%[2]s" TO "%[3]s"'
gunzip -c "$FILE" | pg -d %[2]s -c 'SET ROLE "%[3]s"' -f -
echo "Restored $(runuser -u postgres -- psql -tA -d %[2]s -c "SELECT count(*) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog','information_schema')") tables"`,
		shell.Quote(file), dbName, owner)
	return d.run(ctx, w, script, executor.Options{})
}
