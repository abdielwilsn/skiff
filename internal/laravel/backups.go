package laravel

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"skiff/internal/executor"
	"skiff/internal/shell"
)

// Database backups. A backup is a gzipped dump of one database:
//
//	MySQL:  mysqldump --single-transaction (a consistent snapshot, no locks)
//	SQLite: sqlite3 .backup (a consistent copy, even while the app writes)
//
// Files live in BackupRoot/<source>/<timestamp>-<reason>.<ext>.gz, readable by
// root only, and can also be copied to S3-compatible storage (AWS S3,
// Cloudflare R2, Backblaze B2, DigitalOcean Spaces, MinIO…) with curl's
// built-in AWS signing, so no SDK or extra tool is needed.

// BackupRoot holds every local backup.
const BackupRoot = "/var/backups/skiff"

// SQLite databases Skiff knows where to find, relative to a site's shared/.
var sqliteFiles = []string{"database/database.sqlite", "db.sqlite3"}

var (
	backupFileRe = regexp.MustCompile(`^` + regexp.QuoteMeta(BackupRoot) + `/[a-z0-9._-]{1,120}/[0-9]{8}-[0-9]{6}-[a-z]{1,12}\.(sql|sqlite)\.gz$`)
	s3KeyCharsRe = regexp.MustCompile(`^[A-Za-z0-9/+=._-]{1,256}$`)
	bucketRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionRe     = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	prefixRe     = regexp.MustCompile(`^[A-Za-z0-9._/-]{0,100}$`)
	objectKeyRe  = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,500}$`)
)

// ValidBackupFile reports whether path is a backup file Skiff created.
func ValidBackupFile(path string) bool {
	return backupFileRe.MatchString(path) && !strings.Contains(path, "..")
}

// SQLitePath is the absolute path of a site's shared SQLite database.
func SQLitePath(domain, rel string) (string, bool) {
	for _, f := range sqliteFiles {
		if f == rel {
			return SitePath(domain) + "/shared/" + rel, true
		}
	}
	return "", false
}

// S3 is an S3-compatible bucket for off-site copies.
type S3 struct {
	Endpoint  string // e.g. https://s3.us-east-1.amazonaws.com or https://<account>.r2.cloudflarestorage.com
	Region    string // e.g. us-east-1; "auto" for R2
	Bucket    string
	Prefix    string // folder inside the bucket, e.g. "skiff"
	AccessKey string
	SecretKey string
}

// Configured reports whether off-site storage is set up.
func (c S3) Configured() bool { return c.Endpoint != "" && c.Bucket != "" && c.AccessKey != "" }

// Validate checks the settings are well-formed (and safe to put in a curl config).
func (c S3) Validate() error {
	ep := strings.TrimRight(c.Endpoint, "/")
	switch {
	case !strings.HasPrefix(ep, "https://") && !strings.HasPrefix(ep, "http://"):
		return fmt.Errorf("the endpoint must be a URL like https://s3.us-east-1.amazonaws.com")
	case strings.ContainsAny(ep, " \"'\\\n") || strings.Count(ep, "/") > 2:
		return fmt.Errorf("the endpoint must be just the server address, without a bucket or path")
	case !bucketRe.MatchString(c.Bucket):
		return fmt.Errorf("that bucket name isn't valid")
	case !regionRe.MatchString(c.Region):
		return fmt.Errorf("enter a region, e.g. us-east-1 (or auto for Cloudflare R2)")
	case !prefixRe.MatchString(c.Prefix) || strings.Contains(c.Prefix, ".."):
		return fmt.Errorf("the folder may contain letters, digits and . _ / - only")
	case !s3KeyCharsRe.MatchString(c.AccessKey) || !s3KeyCharsRe.MatchString(c.SecretKey):
		return fmt.Errorf("enter the access key ID and secret access key")
	}
	return nil
}

// objectURL is the path-style URL of an object (supported by every provider).
func (c S3) objectURL(key string) string {
	return strings.TrimRight(c.Endpoint, "/") + "/" + c.Bucket + "/" + key
}

// ObjectKey is where a backup file goes in the bucket: <prefix>/<host>/<source>/<file>.
func (c S3) ObjectKey(host, file string) string {
	parts := strings.Split(file, "/")
	name := strings.Join(parts[len(parts)-2:], "/")
	p := strings.Trim(c.Prefix, "/")
	if p != "" {
		p += "/"
	}
	return p + host + "/" + name
}

// curlConfig carries the credentials to curl on stdin, so they never appear
// in a process list or a log.
func (c S3) curlConfig() string {
	return fmt.Sprintf("user = \"%s:%s\"\naws-sigv4 = \"aws:amz:%s:s3\"\n", c.AccessKey, c.SecretKey, c.Region)
}

// sizeRe picks the "SIZE=<bytes>" line backup scripts print last.
var sizeRe = regexp.MustCompile(`(?m)^SIZE=([0-9]+)$`)

func (d *Deployer) runSized(ctx context.Context, w io.Writer, script string, opt executor.Options) (int64, error) {
	var buf bytes.Buffer
	err := d.run(ctx, io.MultiWriter(w, &buf), script, opt)
	if err != nil {
		return 0, err
	}
	m := sizeRe.FindAllStringSubmatch(buf.String(), -1)
	if len(m) == 0 {
		return 0, nil
	}
	n, _ := strconv.ParseInt(m[len(m)-1][1], 10, 64)
	return n, nil
}

const backupPrelude = `umask 077
FILE=%[1]s
DIR="$(dirname "$FILE")"
mkdir -p "$DIR"
chmod 700 %[2]s "$DIR"
TMP="$FILE.part"
trap 'rm -f "$TMP"' EXIT
`

const backupFinish = `mv -f "$TMP" "$FILE"
SIZE=$(stat -c %s "$FILE")
echo "Saved $FILE ($(numfmt --to=iec "$SIZE" 2>/dev/null || echo "$SIZE bytes"))"
echo "SIZE=$SIZE"
`

// BackupMySQL dumps a MySQL database to file and returns its size.
func (d *Deployer) BackupMySQL(ctx context.Context, w io.Writer, dbName, file string) (int64, error) {
	if !ValidDBName(dbName) || !ValidBackupFile(file) {
		return 0, fmt.Errorf("invalid backup target")
	}
	step(w, "Backing up MySQL database %s", dbName)
	script := fmt.Sprintf(backupPrelude, shell.Quote(file), BackupRoot) + fmt.Sprintf(`command -v mysqldump >/dev/null 2>&1 || { echo "MySQL isn't installed." >&2; exit 1; }
mysqldump --user=root --single-transaction --quick --routines --triggers --events --no-tablespaces --set-gtid-purged=OFF %s | gzip -6 > "$TMP"
`, dbName) + backupFinish
	return d.runSized(ctx, w, script, executor.Options{})
}

// BackupSQLite copies a SQLite database consistently to file and returns its
// size. The database is read by the site user, which owns it: reading it as
// root would follow a symlink the site planted to any root-only file (such as
// Skiff's own database). Root only writes the gzipped stream into the backup.
func (d *Deployer) BackupSQLite(ctx context.Context, w io.Writer, dbPath, file string) (int64, error) {
	if !strings.HasPrefix(dbPath, SitesRoot+"/") || strings.Contains(dbPath, "..") || !ValidBackupFile(file) {
		return 0, fmt.Errorf("invalid backup target")
	}
	step(w, "Backing up SQLite database %s", dbPath)
	script := fmt.Sprintf(backupPrelude, shell.Quote(file), BackupRoot) + fmt.Sprintf(`DB=%[1]s
if ! command -v sqlite3 >/dev/null 2>&1; then
    echo "Installing the sqlite3 command-line tool"
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q sqlite3 >/dev/null
fi
runuser -u %[2]s -- env -i PATH=/usr/bin:/bin HOME=%[3]s bash -c '
set -eo pipefail
umask 077
[ -s "$1" ] || { echo "$1 does not exist or is empty." >&2; exit 1; }
COPY="$(mktemp)"
trap "rm -f \"\$COPY\"" EXIT
sqlite3 "$1" ".timeout 10000" ".backup $COPY" >&2
gzip -6 -c "$COPY"' _ "$DB" > "$TMP"
`, shell.Quote(dbPath), SiteUser, shell.Quote(SitesRoot)) + backupFinish
	return d.runSized(ctx, w, script, executor.Options{})
}

// SQLiteDatabases lists the non-empty shared SQLite databases of these sites,
// as domain -> paths relative to shared/.
func (d *Deployer) SQLiteDatabases(ctx context.Context, domains []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(domains) == 0 {
		return out, nil
	}
	var b strings.Builder
	for _, dom := range domains {
		for _, f := range sqliteFiles {
			p, _ := SQLitePath(dom, f)
			fmt.Fprintf(&b, "if [ -s %s ]; then printf 'DB\\t%%s\\t%%s\\n' %s %s; fi\n", shell.Quote(p), shell.Quote(dom), shell.Quote(f))
		}
	}
	var buf bytes.Buffer
	// As the site user: a symlinked database must not be followed as root.
	if err := d.Exec.Run(ctx, b.String(), executor.Options{User: SiteUser}, &buf); err != nil {
		return out, err
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) == 3 && parts[0] == "DB" {
			out[parts[1]] = append(out[parts[1]], parts[2])
		}
	}
	return out, nil
}

// Upload copies a backup file to the bucket.
func (d *Deployer) Upload(ctx context.Context, w io.Writer, c S3, file, key string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if !ValidBackupFile(file) || !objectKeyRe.MatchString(key) || strings.Contains(key, "..") {
		return fmt.Errorf("invalid upload")
	}
	step(w, "Uploading to %s/%s", c.Bucket, key)
	script := fmt.Sprintf(`FILE=%s
SHA=$(sha256sum "$FILE" | awk '{print $1}')
curl -fsS --retry 3 --retry-all-errors --max-time 3600 -K - -H "x-amz-content-sha256: $SHA" -T "$FILE" %s -o /dev/null
echo "Uploaded"`, shell.Quote(file), shell.Quote(c.objectURL(key)))
	return d.run(ctx, w, script, executor.Options{Stdin: strings.NewReader(c.curlConfig())})
}

// Download fetches a backup from the bucket to file (used to restore a
// backup whose local copy is gone).
func (d *Deployer) Download(ctx context.Context, w io.Writer, c S3, key, file string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if !ValidBackupFile(file) || !objectKeyRe.MatchString(key) || strings.Contains(key, "..") {
		return fmt.Errorf("invalid download")
	}
	step(w, "Downloading %s/%s", c.Bucket, key)
	script := fmt.Sprintf(`umask 077
FILE=%s
mkdir -p "$(dirname "$FILE")"
curl -fsS --retry 3 --retry-all-errors --max-time 3600 -K - -H "x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" %s -o "$FILE.part"
mv -f "$FILE.part" "$FILE"
echo "Downloaded $(stat -c %%s "$FILE") bytes"`, shell.Quote(file), shell.Quote(c.objectURL(key)))
	return d.run(ctx, w, script, executor.Options{Stdin: strings.NewReader(c.curlConfig())})
}

// DeleteRemote removes a backup from the bucket (missing objects are fine).
func (d *Deployer) DeleteRemote(ctx context.Context, w io.Writer, c S3, key string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if !objectKeyRe.MatchString(key) || strings.Contains(key, "..") {
		return fmt.Errorf("invalid key")
	}
	script := fmt.Sprintf(`curl -fsS --retry 2 -K - -X DELETE -H "x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" %s -o /dev/null`, shell.Quote(c.objectURL(key)))
	return d.run(ctx, w, script, executor.Options{Stdin: strings.NewReader(c.curlConfig())})
}

// TestStorage writes, reads back and deletes a small object.
func (d *Deployer) TestStorage(ctx context.Context, w io.Writer, c S3, host string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	key := c.ObjectKey(host, "x/skiff-connection-test.txt")
	empty := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	script := fmt.Sprintf(`URL=%[1]s
BODY="Skiff connection test $(date -u +%%FT%%TZ)"
SHA=$(printf '%%s' "$BODY" | sha256sum | awk '{print $1}')
CFG="$(cat)"
echo "Writing $URL"
printf '%%s' "$BODY" | curl -fsS -K <(printf '%%s\n' "$CFG") -H "x-amz-content-sha256: $SHA" -T - "$URL" -o /dev/null
echo "Reading it back"
GOT="$(curl -fsS -K <(printf '%%s\n' "$CFG") -H "x-amz-content-sha256: %[2]s" "$URL")"
[ "$GOT" = "$BODY" ] || { echo "Read back something else: $GOT" >&2; exit 1; }
echo "Deleting it"
curl -fsS -K <(printf '%%s\n' "$CFG") -X DELETE -H "x-amz-content-sha256: %[2]s" "$URL" -o /dev/null
echo "Storage works."`, shell.Quote(c.objectURL(key)), empty)
	return d.run(ctx, w, script, executor.Options{Stdin: strings.NewReader(c.curlConfig())})
}

// RemoveBackupFile deletes a local backup file (a missing file is fine).
func (d *Deployer) RemoveBackupFile(ctx context.Context, w io.Writer, file string) error {
	if !ValidBackupFile(file) {
		return fmt.Errorf("invalid backup file")
	}
	return d.run(ctx, w, "rm -f -- "+shell.Quote(file), executor.Options{})
}

// RestoreMySQL replaces a MySQL database's contents with a backup. The
// database is dropped and recreated first, so tables added since the backup
// don't linger; the user's privileges are kept. The dump is loaded by nobody,
// connected as the database's own user (credentials over stdin, never on a
// command line), in --binary-mode so client commands like "\!" are refused:
// an imported dump can't reach other databases or run anything as root.
func (d *Deployer) RestoreMySQL(ctx context.Context, w io.Writer, dbName, user, password, file string) error {
	if !ValidDBName(dbName) || !ValidDBName(user) || !dbPasswordRe.MatchString(password) || !ValidBackupFile(file) || !strings.HasSuffix(file, ".sql.gz") {
		return fmt.Errorf("invalid restore")
	}
	step(w, "Restoring MySQL database %s from %s", dbName, file)
	script := mysqlCleanFn + fmt.Sprintf("FILE=%[1]s\n"+`IFS= read -r DBUSER
IFS= read -r DBPASS
[ -f "$FILE" ] || { echo "The backup file is missing." >&2; exit 1; }
gzip -t "$FILE"
# Lets the database's own user create triggers and stored functions while
# binary logging is on (it's a single server, so nothing replicates them).
mysql --user=root -e 'SET PERSIST log_bin_trust_function_creators = 1' 2>/dev/null || true
# Single quotes: the backticks are SQL identifier quotes, not shell.
mysql --user=root -e 'DROP DATABASE IF EXISTS `+"`%[2]s`"+`; CREATE DATABASE `+"`%[2]s`"+` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;'
cd /
# nobody reads the password from descriptor 3 and hands it to mysql in an
# option file on a pipe of its own, so it's never in an argument list.
gunzip -c "$FILE" | mysql_clean | runuser -u nobody -- env -i PATH=/usr/bin:/bin HOME=/nonexistent bash -c \
    'IFS= read -r PW <&3; exec 3<&-; exec mysql --defaults-file=<(printf "[client]\nuser=%%s\npassword=%%s\n" "$1" "$PW") --protocol=TCP --host=127.0.0.1 --binary-mode --default-character-set=utf8mb4 "$2"' \
    _ "$DBUSER" %[2]s 3<<<"$DBPASS"
echo "Restored $(mysql --user=root -N -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='%[2]s'") tables"`, shell.Quote(file), dbName)
	return d.run(ctx, w, script, executor.Options{Stdin: strings.NewReader(user + "\n" + password + "\n")})
}

// RestoreSQLite replaces a site's SQLite database with a backup (an atomic
// rename, so the app never sees a half-written file). Root only reads the
// backup; the site user writes it into place, so nothing is written through a
// symlink the site controls with root's permissions.
func (d *Deployer) RestoreSQLite(ctx context.Context, w io.Writer, dbPath, file string) error {
	if !strings.HasPrefix(dbPath, SitesRoot+"/") || strings.Contains(dbPath, "..") || !ValidBackupFile(file) || !strings.HasSuffix(file, ".sqlite.gz") {
		return fmt.Errorf("invalid restore")
	}
	step(w, "Restoring %s from %s", dbPath, file)
	script := fmt.Sprintf(`set -o pipefail
FILE=%[1]s
[ -f "$FILE" ] || { echo "The backup file is missing." >&2; exit 1; }
gzip -t "$FILE"
gunzip -c "$FILE" | runuser -u %[3]s -- env -i PATH=/usr/bin:/bin HOME=%[4]s bash -c '
set -e
DB="$1"
mkdir -p "$(dirname "$DB")"
TMP="$(mktemp "$(dirname "$DB")/.restore.XXXXXX")"
trap "rm -f \"\$TMP\"" EXIT
cat > "$TMP"
chmod 644 "$TMP"
mv -f "$TMP" "$DB"
rm -f "$DB-wal" "$DB-shm" "$DB-journal"
echo "Restored $DB ($(stat -c %%s "$DB") bytes)"' _ %[2]s`, shell.Quote(file), shell.Quote(dbPath), SiteUser, shell.Quote(SitesRoot))
	return d.run(ctx, w, script, executor.Options{})
}
