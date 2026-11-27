package laravel

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"skiff/internal/executor"
	"skiff/internal/shell"
)

// Importing a database (moving from another server, say) turns a dump into a
// regular Skiff backup of the target database and restores it, so an import
// gets the same safety backup and checks as any restore and can be restored
// again later.
//
// Accepted: SQL dumps (mysqldump, MariaDB, pg_dump, phpMyAdmin, TablePlus…),
// PostgreSQL custom and tar archives (pg_dump -Fc/-Ft), SQLite files, each
// optionally gzipped, or a zip holding exactly one of them (Laravel backup
// archives, for example).
//
// Dumps come from anywhere, so they are never trusted: the SQL is loaded by
// an unprivileged OS user connecting as the site's own database user, which
// can only touch that one database (see RestoreMySQL and RestorePostgres).

// ImportRoot is where files to import are uploaded or copied (scp) to. Only
// root can write there, so whatever is in it was put there by root or the
// panel. A var so tests can point it elsewhere.
var ImportRoot = BackupRoot + "/imports"

var importNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,200}$`)

// ValidImportName reports whether name can be a file in ImportRoot.
func ValidImportName(name string) bool {
	return importNameRe.MatchString(name) && !strings.Contains(name, "..")
}

// importScript converts SRC into the gzipped form a restore takes ($KIND:
// mysql or postgres → .sql.gz, sqlite → .sqlite.gz) and writes it to DST.
const importScript = pkgsInstalledFn + `
[ -f "$SRC" ] && [ ! -L "$SRC" ] || { echo "The file to import is missing." >&2; exit 1; }
[ -s "$SRC" ] || { echo "The file to import is empty." >&2; exit 1; }
umask 077
mkdir -p "$(dirname "$DST")"
chmod 700 "$BACKUP_ROOT" "$(dirname "$DST")"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# hexhead FILE N: the first N bytes of FILE in hex.
hexhead() { head -c "$2" "$1" | od -An -tx1 | tr -d ' \n'; }
IN="$SRC"
echo "File: $(basename "$SRC"), $(numfmt --to=iec "$(stat -c %s "$SRC")" 2>/dev/null || stat -c %s "$SRC") bytes"

# A zip: use the one database file inside.
case "$(hexhead "$IN" 4)" in 504b0304*)
    command -v unzip >/dev/null 2>&1 || { export DEBIAN_FRONTEND=noninteractive; apt_install unzip >/dev/null; }
    ENTRIES="$(unzip -Z1 "$IN" | grep -Ei '\.(sql|sql\.gz|dump|pgdump|backup|tar|sqlite|sqlite3|db)$' || true)"
    COUNT=$(printf '%s' "$ENTRIES" | grep -c . || true)
    if [ "$COUNT" != 1 ]; then
        echo "The zip holds $COUNT database files; Skiff needs exactly one. Unzip it and import the dump itself." >&2
        [ -n "$ENTRIES" ] && printf '  %s\n' $ENTRIES >&2
        exit 1
    fi
    echo "Using $ENTRIES from the zip"
    unzip -p "$IN" "$ENTRIES" > "$WORK/unzipped"
    IN="$WORK/unzipped"
    ;;
esac

# Gzipped: check it's whole, then look at what's inside.
GZ=0
if [ "$(hexhead "$IN" 2)" = 1f8b ]; then
    GZ=1
    gzip -t "$IN" 2>/dev/null || { echo "The gzip file is damaged or incomplete (was the upload or copy cut short?)." >&2; exit 1; }
fi
content() { if [ "$GZ" = 1 ]; then gunzip -c "$IN"; else cat "$IN"; fi; }
content | head -c 65536 > "$WORK/head" || true

FORMAT=sql
case "$(hexhead "$WORK/head" 16)" in
    53514c69746520666f726d6174203300) FORMAT=sqlite ;;  # "SQLite format 3\0"
    5047444d50*) FORMAT=pgarchive ;;                     # "PGDMP": pg_dump -Fc
esac
[ "$(tail -c +258 "$WORK/head" | head -c 5 | tr -d '\0')" = ustar ] && FORMAT=pgarchive  # pg_dump -Ft

case "$KIND:$FORMAT" in
    sqlite:sqlite) ;;
    sqlite:*) echo "That isn't a SQLite database file." >&2; exit 1 ;;
    *:sqlite) echo "That's a SQLite database file; import it into a SQLite database." >&2; exit 1 ;;
    mysql:pgarchive) echo "That's a PostgreSQL archive (pg_dump -Fc or -Ft); import it into a PostgreSQL database." >&2; exit 1 ;;
esac
if [ "$FORMAT" = sql ]; then
    if grep -aq -m1 'PostgreSQL database dump' "$WORK/head" && [ "$KIND" = mysql ]; then
        echo "That's a PostgreSQL dump; import it into a PostgreSQL database." >&2; exit 1
    fi
    if grep -aqE -m1 '^-- (MySQL|MariaDB) dump|^/\*!40101 SET' "$WORK/head" && [ "$KIND" = postgres ]; then
        echo "That's a MySQL dump; import it into a MySQL database." >&2; exit 1
    fi
fi

TMP="$DST.part"
trap 'rm -rf "$WORK" "$TMP"' EXIT
case "$FORMAT" in
    pgarchive)
        echo "Converting the PostgreSQL archive to SQL with pg_restore"
        command -v pg_restore >/dev/null 2>&1 || { echo "PostgreSQL isn't installed." >&2; exit 1; }
        # pg_restore reads the untrusted archive as nobody.
        content | (cd / && runuser -u nobody -- env -i PATH=/usr/bin:/bin pg_restore --no-owner --no-privileges -f -) | gzip -6 > "$TMP"
        ;;
    *)
        content | gzip -6 > "$TMP"
        ;;
esac
mv -f "$TMP" "$DST"
SIZE=$(stat -c %s "$DST")
echo "Ready to import ($(numfmt --to=iec "$SIZE" 2>/dev/null || echo "$SIZE bytes") gzipped)"
echo "SIZE=$SIZE"
`

// PrepareImport converts src, a file in ImportRoot, into the backup file dst
// for a database of kind ("mysql", "postgres" or "sqlite") and returns its
// size. It checks the file is the right kind of database first.
func (d *Deployer) PrepareImport(ctx context.Context, w io.Writer, kind, src, dst string) (int64, error) {
	ext := ".sql.gz"
	if kind == "sqlite" {
		ext = ".sqlite.gz"
	} else if kind != "mysql" && kind != "postgres" {
		return 0, fmt.Errorf("unknown database kind %q", kind)
	}
	if filepath.Dir(src) != ImportRoot || !ValidImportName(filepath.Base(src)) {
		return 0, fmt.Errorf("invalid file to import")
	}
	if !ValidBackupFile(dst) || !strings.HasSuffix(dst, ext) {
		return 0, fmt.Errorf("invalid import target")
	}
	step(w, "Preparing %s for import", filepath.Base(src))
	script := fmt.Sprintf("SRC=%s\nDST=%s\nKIND=%s\nBACKUP_ROOT=%s\n", shell.Quote(src), shell.Quote(dst), kind, shell.Quote(BackupRoot)) + importScript
	return d.runSized(ctx, w, script, executor.Options{})
}

// mysqlCleanFn defines mysql_clean, a filter that makes a dump from another
// server load into this database as its own user: statements that pick or
// create another database, need server-wide privileges (GTIDs, binary log)
// or name another server's users (DEFINER=) are dropped. Data lines are left
// alone.
const mysqlCleanFn = `mysql_clean() {
    sed -E '/^(INSERT|REPLACE) INTO /!{
        /^SET @@GLOBAL\.GTID_PURGED/{
            :gtid
            /;[[:space:]]*$/!{
                N
                bgtid
            }
            d
        }
        /^(CREATE DATABASE |USE ` + "`" + `|SET @MYSQLDUMP_TEMP_LOG_BIN|SET @@SESSION\.SQL_LOG_BIN)/d
        /^\/\*M!999999\\- enable the sandbox mode \*\/[[:space:]]*$/d
        /^(\/\*!|CREATE |ALTER )/s/DEFINER=(` + "`[^`]*`" + `|[^ @*]+)@(` + "`[^`]*`" + `|[^ *]+) ?//g
    }'
}
`

var dbPasswordRe = regexp.MustCompile(`^[A-Za-z0-9]{8,128}$`)

// pgCleanFn defines pg_clean, the same for PostgreSQL: ownership and grants
// for roles that don't exist here, statements about other databases or the
// public schema (which the database's owner already has), and settings older
// servers don't know are dropped. COPY data is passed through untouched.
const pgCleanFn = `pg_clean() {
    awk '
        incopy { print; if ($0 == "\\.") incopy = 0; next }
        /^COPY .* FROM stdin;$/ { incopy = 1; print; next }
        /^ALTER .* OWNER TO [^ ]+;$/ { next }
        /^(GRANT|REVOKE) / { next }
        /^(CREATE|ALTER|DROP) DATABASE / { next }
        /^\\(connect|c|restrict|unrestrict) / { next }
        /^COMMENT ON (EXTENSION|DATABASE|SCHEMA public) / { next }
        /^CREATE SCHEMA public;$/ { next }
        /^SET transaction_timeout / { next }
        { print }'
}
# pg_extensions lists the extensions a dump creates.
pg_extensions() {
    { grep -aoE '^CREATE EXTENSION (IF NOT EXISTS )?"?[a-z0-9_]+' || true; } | awk '{ gsub(/"/, "", $NF); print $NF }' | sort -u
}
`
