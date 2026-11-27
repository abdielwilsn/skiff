package laravel

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"skiff/internal/executor"
)

var testS3 = S3{Endpoint: "https://s3.us-east-1.amazonaws.com", Region: "us-east-1", Bucket: "my-backups",
	Prefix: "skiff", AccessKey: "AKIAEXAMPLEKEY", SecretKey: "example/Secret+Value="}

func TestBackupFilePaths(t *testing.T) {
	good := "/var/backups/skiff/mysql-app_example_com/20261123-030000-scheduled.sql.gz"
	if !ValidBackupFile(good) || !ValidBackupFile("/var/backups/skiff/sqlite-a.example.com-db.sqlite3/20261123-030000-deploy.sqlite.gz") {
		t.Error("valid backup paths rejected")
	}
	for _, bad := range []string{"/etc/passwd", "/var/backups/skiff/../x/20261123-030000-a.sql.gz", "/var/backups/skiff/a/b/20261123-030000-a.sql.gz", "/var/backups/skiff/a/x.sql.gz", good + " ; rm"} {
		if ValidBackupFile(bad) {
			t.Errorf("ValidBackupFile(%q) = true", bad)
		}
	}
	if p, ok := SQLitePath("a.example.com", "db.sqlite3"); !ok || p != "/home/skiff/a.example.com/shared/db.sqlite3" {
		t.Errorf("SQLitePath = %q %v", p, ok)
	}
	if _, ok := SQLitePath("a.example.com", "../../etc/shadow"); ok {
		t.Error("SQLitePath accepted an arbitrary path")
	}
}

func TestS3Settings(t *testing.T) {
	if err := testS3.Validate(); err != nil {
		t.Fatal(err)
	}
	if k := testS3.ObjectKey("web-1", "/var/backups/skiff/mysql-app/20261123-030000-scheduled.sql.gz"); k != "skiff/web-1/mysql-app/20261123-030000-scheduled.sql.gz" {
		t.Errorf("ObjectKey = %q", k)
	}
	for _, mut := range []func(*S3){
		func(c *S3) { c.Endpoint = "s3.amazonaws.com" },
		func(c *S3) { c.Endpoint = "https://s3.amazonaws.com/bucket" },
		func(c *S3) { c.Bucket = "Bad_Bucket" },
		func(c *S3) { c.SecretKey = `x" -o /etc/passwd "` },
		func(c *S3) { c.Prefix = "../up" },
	} {
		c := testS3
		mut(&c)
		if c.Validate() == nil {
			t.Errorf("Validate accepted %+v", c)
		}
	}
}

// sizedExec answers backup scripts with a size line.
type sizedExec struct{ recorder }

func (e *sizedExec) Run(ctx context.Context, script string, opt executor.Options, w io.Writer) error {
	_ = e.recorder.Run(ctx, script, opt, w)
	if strings.Contains(script, `echo "SIZE=$SIZE"`) {
		io.WriteString(w, "Saved\nSIZE=12345\n")
	}
	return nil
}

func TestBackupScripts(t *testing.T) {
	e := &sizedExec{}
	d := &Deployer{Exec: e}
	file := "/var/backups/skiff/mysql-app/20261123-030000-manual.sql.gz"
	n, err := d.BackupMySQL(context.Background(), &bytes.Buffer{}, "app", file)
	if err != nil || n != 12345 {
		t.Fatalf("BackupMySQL = %d, %v", n, err)
	}
	if s := e.scripts[0]; !strings.Contains(s, "mysqldump --user=root --single-transaction") || !strings.Contains(s, "umask 077") || !strings.Contains(s, `mv -f "$TMP" "$FILE"`) {
		t.Errorf("mysql backup script:\n%s", s)
	}
	if _, err := d.BackupMySQL(context.Background(), &bytes.Buffer{}, "app; drop", file); err == nil {
		t.Error("unsafe database name accepted")
	}
	if _, err := d.BackupSQLite(context.Background(), &bytes.Buffer{}, "/etc/shadow", "/var/backups/skiff/x/20261123-030000-manual.sqlite.gz"); err == nil {
		t.Error("SQLite backup of a file outside the sites accepted")
	}

	// Credentials travel on stdin only.
	rec := &recorder{}
	d = &Deployer{Exec: rec}
	if err := d.Upload(context.Background(), &bytes.Buffer{}, testS3, file, testS3.ObjectKey("web-1", file)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.scripts[0], testS3.SecretKey) || strings.Contains(rec.scripts[0], testS3.AccessKey) {
		t.Error("S3 credentials in the upload script")
	}
	if !strings.Contains(rec.stdin[0], `user = "AKIAEXAMPLEKEY:example/Secret+Value="`) || !strings.Contains(rec.stdin[0], `aws-sigv4 = "aws:amz:us-east-1:s3"`) {
		t.Errorf("curl config = %q", rec.stdin[0])
	}
	if !strings.Contains(rec.scripts[0], "https://s3.us-east-1.amazonaws.com/my-backups/skiff/web-1/mysql-app/20261123-030000-manual.sql.gz") {
		t.Errorf("upload URL wrong:\n%s", rec.scripts[0])
	}
}

func TestRestoreScripts(t *testing.T) {
	rec := &recorder{}
	d := &Deployer{Exec: rec}
	if err := d.RestoreMySQL(context.Background(), &bytes.Buffer{}, "app", "app", "Secret123pass", "/var/backups/skiff/mysql-app/20261123-030000-manual.sql.gz"); err != nil {
		t.Fatal(err)
	}
	// Loaded by nobody as the database's own user, client commands off; the
	// password only goes over stdin.
	if s := rec.scripts[0]; !strings.Contains(s, "-e 'DROP DATABASE IF EXISTS `app`; CREATE DATABASE `app`") ||
		!strings.Contains(s, `gunzip -c "$FILE" | mysql_clean | runuser -u nobody --`) || !strings.Contains(s, "--binary-mode") ||
		strings.Contains(s, "Secret123pass") || rec.stdin[0] != "app\nSecret123pass\n" {
		t.Errorf("mysql restore:\n%s", s)
	}
	if err := d.RestoreSQLite(context.Background(), &bytes.Buffer{}, "/home/skiff/a.example.com/shared/db.sqlite3", "/var/backups/skiff/sqlite-a/20261123-030000-manual.sqlite.gz"); err != nil {
		t.Fatal(err)
	}
	if s := rec.scripts[1]; !strings.Contains(s, `mv -f "$TMP" "$DB"`) || !strings.Contains(s, `rm -f "$DB-wal" "$DB-shm"`) {
		t.Errorf("sqlite restore:\n%s", s)
	}
	if err := d.RestoreMySQL(context.Background(), &bytes.Buffer{}, "app", "app", "Secret123pass", "/var/backups/skiff/sqlite-a/20261123-030000-manual.sqlite.gz"); err == nil {
		t.Error("restoring a SQLite file into MySQL accepted")
	}
}
