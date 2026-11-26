package laravel

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"skiff/internal/store"
)

func TestPostgresDatabases(t *testing.T) {
	rec := &recorder{}
	d := &Deployer{Exec: rec}
	pw := NewPassword()
	spec := DBSpec{Engine: store.EnginePostgres, Name: "shop", User: "shop", Password: pw}
	var log bytes.Buffer
	ctx := context.Background()

	if err := d.CreateDatabase(ctx, &log, spec); err != nil {
		t.Fatal(err)
	}
	if in := rec.stdin[0]; !strings.Contains(in, "CREATE ROLE %I LOGIN") || !strings.Contains(in, "CREATE DATABASE %I OWNER %I") ||
		!strings.Contains(in, `REVOKE ALL ON DATABASE "shop" FROM PUBLIC`) || !strings.Contains(in, pw) {
		t.Errorf("create SQL:\n%s", in)
	}
	if !strings.Contains(rec.scripts[0], "runuser -u postgres -- psql") {
		t.Errorf("create script: %s", rec.scripts[0])
	}

	// Laravel gets DB_* keys for pgsql; other apps a DATABASE_URL.
	if err := d.WriteDatabaseEnv(ctx, &log, store.Site{Domain: "shop.example.com", Kind: store.KindLaravel}, spec); err != nil {
		t.Fatal(err)
	}
	if s := rec.scripts[1]; !strings.Contains(s, "set_env DB_CONNECTION pgsql") || !strings.Contains(s, "set_env DB_PORT 5432") || strings.Contains(s, "DATABASE_URL") {
		t.Errorf("laravel env script: %s", s)
	}
	if err := d.WriteDatabaseEnv(ctx, &log, store.Site{Domain: "dj.example.com", Kind: store.KindPython}, spec); err != nil {
		t.Fatal(err)
	}
	if s := rec.scripts[2]; !strings.Contains(s, `set_env DATABASE_URL "postgres://shop:$SKIFF_DB_PASSWORD@127.0.0.1:5432/shop"`) || strings.Contains(s, "DB_CONNECTION") {
		t.Errorf("python env script: %s", s)
	}
	mysql := DBSpec{Name: "shop", User: "shop", Password: pw}
	_ = d.WriteDatabaseEnv(ctx, &log, store.Site{Domain: "n.example.com", Kind: store.KindNode}, mysql)
	if s := rec.scripts[3]; !strings.Contains(s, "mysql://shop:$SKIFF_DB_PASSWORD@127.0.0.1:3306/shop") {
		t.Errorf("node env script: %s", s)
	}

	file := BackupRoot + "/postgres-shop/20261124-030000-manual.sql.gz"
	if _, err := d.BackupPostgres(ctx, &log, "shop", file); err != nil {
		t.Fatal(err)
	}
	if err := d.RestorePostgres(ctx, &log, "shop", "shop", file); err != nil {
		t.Fatal(err)
	}
	if err := d.DropDatabase(ctx, &log, spec); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(rec.scripts, "\n")
	for _, want := range []string{"pg_dump --no-owner --no-privileges shop", `WITH (FORCE)`, `CREATE DATABASE \"shop\" OWNER \"shop\"`, `-c 'SET ROLE "shop"' -f -`} {
		if !strings.Contains(all, want) {
			t.Errorf("scripts never ran %q", want)
		}
	}
	if !strings.Contains(rec.stdin[len(rec.stdin)-1], `DROP ROLE IF EXISTS "shop"`) {
		t.Error("drop should remove the role")
	}
	if strings.Contains(all, pw) || strings.Contains(log.String(), pw) {
		t.Error("the password leaked into a script or the log")
	}
	if err := d.RestorePostgres(ctx, &log, "shop", "x'; DROP", file); err == nil {
		t.Error("accepted a bad owner")
	}
	for _, bad := range []string{"postgres", "pg_shop", "template1"} {
		if ValidDBName(bad) {
			t.Errorf("%q should be reserved", bad)
		}
	}
	if n := DBNameFor("pg_stats.example.com"); !ValidDBName(n) {
		t.Errorf("DBNameFor gave invalid %q", n)
	}
}

func TestRedis(t *testing.T) {
	rec := &recorder{}
	d := &Deployer{Exec: rec}
	pw := NewPassword()
	ctx := context.Background()
	var log bytes.Buffer
	if err := d.InstallRedis(ctx, &log, "bad pass"); err == nil {
		t.Error("accepted a bad password")
	}
	if err := d.InstallRedis(ctx, &log, pw); err != nil {
		t.Fatal(err)
	}
	if s := rec.scripts[0]; !strings.Contains(s, "requirepass $SKIFF_REDIS_PASSWORD") || !strings.Contains(s, "bind 127.0.0.1") || strings.Contains(s, pw) {
		t.Errorf("install script: %s", s)
	}
	lara := store.Site{Domain: "app.example.com", Kind: store.KindLaravel, PHPVersion: "8.4"}
	if err := d.WriteRedisEnv(ctx, &log, lara, pw, true); err != nil {
		t.Fatal(err)
	}
	if s := rec.scripts[1]; !strings.Contains(s, "set_env REDIS_PREFIX app_example_com_") || !strings.Contains(s, "set_env QUEUE_CONNECTION redis") ||
		!strings.Contains(s, "set_env CACHE_STORE redis") || strings.Contains(s, pw) {
		t.Errorf("laravel redis env: %s", s)
	}
	_ = d.WriteRedisEnv(ctx, &log, lara, pw, false)
	if s := rec.scripts[2]; strings.Contains(s, "QUEUE_CONNECTION") {
		t.Error("drivers changed without asking")
	}
	_ = d.WriteRedisEnv(ctx, &log, store.Site{Domain: "n.example.com", Kind: store.KindNode}, pw, true)
	if s := rec.scripts[3]; !strings.Contains(s, `set_env REDIS_URL "redis://default:$SKIFF_REDIS_PASSWORD@127.0.0.1:6379/0"`) || strings.Contains(s, "REDIS_PREFIX") {
		t.Errorf("node redis env: %s", s)
	}

	// A site created with Redis: the extension is installed and the .env
	// written before the deploy script runs.
	f := &failAt{}
	site := testSite
	site.Redis = true
	if err := (&Deployer{Exec: f}).DeployWith(ctx, &bytes.Buffer{}, site, "", DeployOptions{RedisPassword: pw}); err != nil {
		t.Fatal(err)
	}
	envAt, extAt, buildAt := -1, -1, -1
	for i, s := range f.scripts {
		switch {
		case strings.Contains(s, "set_env REDIS_HOST") && envAt < 0:
			envAt = i
		case strings.Contains(s, `grep -x redis`) && extAt < 0:
			extAt = i
		case strings.Contains(s, "BUILD-STEP"):
			buildAt = i
		}
	}
	if envAt < 0 || extAt < 0 || buildAt < envAt || buildAt < extAt {
		t.Errorf("redis env at %d, extension at %d, deploy script at %d", envAt, extAt, buildAt)
	}
}
