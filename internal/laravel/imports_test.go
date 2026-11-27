package laravel

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestPrepareImportValidates(t *testing.T) {
	rec := &recorder{}
	d := &Deployer{Exec: rec}
	ctx := context.Background()
	dst := BackupRoot + "/mysql-app/20261127-120000-import.sql.gz"
	for _, c := range []struct{ kind, src, dst string }{
		{"mysql", "/etc/shadow", dst},                        // outside ImportRoot
		{"mysql", ImportRoot + "/../../etc/shadow", dst},     // escapes it
		{"mysql", ImportRoot + "/sub/dump.sql", dst},         // not directly in it
		{"redis", ImportRoot + "/dump.sql", dst},             // unknown kind
		{"sqlite", ImportRoot + "/dump.sqlite", dst},         // sqlite into a .sql.gz
		{"mysql", ImportRoot + "/dump.sql", "/tmp/x.sql.gz"}, // target outside backups
		{"mysql", ImportRoot + "/dump.sql; rm -rf /", dst},   // shell metacharacters
	} {
		if _, err := d.PrepareImport(ctx, &bytes.Buffer{}, c.kind, c.src, c.dst); err == nil {
			t.Errorf("PrepareImport(%q, %q, %q) accepted", c.kind, c.src, c.dst)
		}
	}
	if len(rec.scripts) != 0 {
		t.Fatalf("a rejected import ran a script")
	}
	if _, err := d.PrepareImport(ctx, &bytes.Buffer{}, "postgres", ImportRoot+"/forge-2026.sql.gz", BackupRoot+"/postgres-app/20261127-120000-import.sql.gz"); err != nil {
		t.Fatal(err)
	}
	s := rec.scripts[0]
	for _, want := range []string{"KIND=postgres", "SRC='" + ImportRoot + "/forge-2026.sql.gz'", "runuser -u nobody -- env -i PATH=/usr/bin:/bin pg_restore --no-owner --no-privileges -f -", `[ ! -L "$SRC" ]`} {
		if !strings.Contains(s, want) {
			t.Errorf("import script is missing %q", want)
		}
	}
}

func TestValidImportName(t *testing.T) {
	for name, want := range map[string]bool{
		"backup.sql.gz": true, "forge_2026-11-27.dump": true, "laravel+db.zip": true,
		"": false, ".hidden": false, "a/b.sql": false, "..": false, "a..b": false, "dump.sql\n": false, "x y.sql": false,
	} {
		if got := ValidImportName(name); got != want {
			t.Errorf("ValidImportName(%q) = %v", name, got)
		}
	}
}
