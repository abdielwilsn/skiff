package laravel

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"skiff/internal/executor"
	"skiff/internal/store"
)

func TestDBNameFor(t *testing.T) {
	for in, want := range map[string]string{
		"app.example.com": "app_example_com",
		"203.0.113.10":    "db_203_0_113_10",
		"my-shop.dev":     "my_shop_dev",
		"localhost":       "localhost",
		"mysql":           "mysql_db",
		"a-very-long-subdomain-name.example.co.uk": "a_very_long_subdomain_name_e", // 28 chars, room for "_2"
	} {
		got := DBNameFor(in)
		if got != want {
			t.Errorf("DBNameFor(%q) = %q, want %q", in, got, want)
		}
		if !ValidDBName(got) {
			t.Errorf("DBNameFor(%q) = %q is not a valid name", in, got)
		}
	}
}

func TestValidDBName(t *testing.T) {
	for _, n := range []string{"shop", "app_1", "a"} {
		if !ValidDBName(n) {
			t.Errorf("ValidDBName(%q) = false", n)
		}
	}
	for _, n := range []string{"", "1abc", "Shop", "a-b", "a b", "mysql", "root", "x`; DROP", strings.Repeat("a", 33)} {
		if ValidDBName(n) {
			t.Errorf("ValidDBName(%q) = true", n)
		}
	}
}

func TestNewPassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		p := NewPassword()
		if err := (DBSpec{Name: "a", User: "a", Password: p}).valid(); err != nil {
			t.Fatalf("NewPassword produced an unusable password %q: %v", p, err)
		}
		if seen[p] {
			t.Fatal("NewPassword repeated itself")
		}
		seen[p] = true
	}
	if err := (DBSpec{Name: "a", User: "a", Password: "short"}).valid(); err == nil {
		t.Error("short password accepted")
	}
	if err := (DBSpec{Name: "a", User: "a", Password: "abcdefghijklmnop'"}).valid(); err == nil {
		t.Error("password with a quote accepted")
	}
}

// recorder captures what would be run, without running it.
type recorder struct {
	scripts []string
	stdin   []string
	env     [][]string
}

func (r *recorder) Run(_ context.Context, script string, opt executor.Options, _ io.Writer) error {
	r.scripts = append(r.scripts, script)
	r.env = append(r.env, opt.Env)
	in := ""
	if opt.Stdin != nil {
		b, _ := io.ReadAll(opt.Stdin)
		in = string(b)
	}
	r.stdin = append(r.stdin, in)
	return nil
}

func TestPasswordsStayOutOfScriptsAndLogs(t *testing.T) {
	rec := &recorder{}
	d := &Deployer{Exec: rec}
	spec := DBSpec{Name: "shop", User: "shop", Password: NewPassword()}
	var log bytes.Buffer
	site := store.Site{Domain: "shop.example.com"}

	if err := d.CreateDatabase(context.Background(), &log, spec); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteDatabaseEnv(context.Background(), &log, site, spec); err != nil {
		t.Fatal(err)
	}
	for i, s := range rec.scripts {
		if strings.Contains(s, spec.Password) {
			t.Errorf("script %d contains the password (it would show in ps and dry-run logs)", i)
		}
	}
	if strings.Contains(log.String(), spec.Password) {
		t.Error("job log contains the password")
	}
	if !strings.Contains(rec.stdin[0], "IDENTIFIED BY '"+spec.Password+"'") {
		t.Error("CREATE USER should receive the password over stdin")
	}
	if !strings.Contains(strings.Join(rec.env[1], "\n"), "SKIFF_DB_PASSWORD="+spec.Password) {
		t.Error(".env update should receive the password through the environment")
	}
	for _, host := range []string{"'shop'@'localhost'", "'shop'@'127.0.0.1'"} {
		if !strings.Contains(rec.stdin[0], host) {
			t.Errorf("missing grant for %s", host)
		}
	}
}
