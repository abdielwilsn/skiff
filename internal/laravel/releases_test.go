package laravel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"skiff/internal/executor"
	"skiff/internal/store"
)

// failAt runs scripts until one contains marker, which fails.
type failAt struct {
	marker  string
	scripts []string
}

func (f *failAt) Run(_ context.Context, script string, opt executor.Options, _ io.Writer) error {
	f.scripts = append(f.scripts, script)
	if opt.Stdin != nil {
		_, _ = io.ReadAll(opt.Stdin)
	}
	if f.marker != "" && strings.Contains(script, f.marker) {
		return errors.New("exit status 1")
	}
	return nil
}

func (f *failAt) ran(sub string) bool {
	for _, s := range f.scripts {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

var testSite = store.Site{Domain: "app.example.com", Repo: "o/r", Branch: "main", PHPVersion: "8.4", NodeVersion: "24", DeployScript: "BUILD-STEP\n"}

func TestDeployBuildsReleaseThenSwitches(t *testing.T) {
	f := &failAt{}
	if err := (&Deployer{Exec: f}).Deploy(context.Background(), &bytes.Buffer{}, testSite, "", nil); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(f.scripts, "\n----\n")
	order := []string{
		`--work-tree="$REL" checkout -f HEAD`,             // code exported into a new release
		`ln -s "$SH/.env" .env`,                           // shared .env linked in
		`ln -s "$SH/storage" storage`,                     // shared storage linked in
		"BUILD-STEP",                                      // deploy script runs in the release
		".skiff-ok",                                       // marked good only after it succeeded
		"mv -Tf current.next current",                     // atomic switch
		"root /home/skiff/app.example.com/current/public", // nginx follows current
		"Kept $kept release(s)",                           // cleanup
	}
	last := -1
	for _, want := range order {
		i := strings.Index(all, want)
		if i < 0 {
			t.Fatalf("deploy never ran %q", want)
		}
		if i < last {
			t.Fatalf("%q ran out of order", want)
		}
		last = i
	}
	if !strings.Contains(NginxConfig(testSite), "root /home/skiff/app.example.com/current/public;") {
		t.Error("new sites must serve current/public")
	}
}

func TestFailedDeployNeverGoesLive(t *testing.T) {
	for _, failing := range []string{"BUILD-STEP", "Preparing environment", `checkout -f HEAD`} {
		f := &failAt{marker: failing}
		if failing == "Preparing environment" {
			f.marker = `SH="$SITE/shared"` // the prepare script
		}
		var log bytes.Buffer
		err := (&Deployer{Exec: f}).Deploy(context.Background(), &log, testSite, "", nil)
		if err == nil {
			t.Fatalf("%s: deploy should fail", failing)
		}
		if f.ran("mv -Tf current.next current") || f.ran("service nginx reload") {
			t.Fatalf("%s: a failed deploy switched the live site", failing)
		}
		if !f.ran("rm -rf -- '/home/skiff/app.example.com/releases/") {
			t.Fatalf("%s: the unfinished release was not discarded", failing)
		}
	}
}

func TestReleasesParsingAndRollback(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	out := "noise line\n20261122141530\t1\t" + sha + "\tabc1234 feat: staff portal\n20261121093012\t0\t\tdef5678 fix\nbad\t1\tx\n"
	d := &Deployer{Exec: printer(out)}
	list, err := d.Releases(context.Background(), testSite)
	if err != nil || len(list) != 2 {
		t.Fatalf("Releases = %+v, %v", list, err)
	}
	if !list[0].Current || list[0].Revision != "abc1234 feat: staff portal" || list[0].Commit != sha ||
		list[1].Current || list[1].Commit != "" || list[1].Revision != "def5678 fix" || list[0].Built.IsZero() {
		t.Fatalf("parsed %+v", list)
	}
	f := &failAt{}
	if err := (&Deployer{Exec: f}).Rollback(context.Background(), &bytes.Buffer{}, testSite, "20261121093012"); err != nil {
		t.Fatal(err)
	}
	if !f.ran("'releases/20261121093012'/.skiff-ok") || !f.ran("mv -Tf current.next current") || !f.ran("php8.4-fpm reload") {
		t.Fatalf("rollback scripts: %v", f.scripts)
	}
	if err := (&Deployer{Exec: f}).Rollback(context.Background(), &bytes.Buffer{}, testSite, "../../etc"); err == nil {
		t.Fatal("rollback accepted a bad release id")
	}
}

func TestReleaseIDsAreUniqueAndSorted(t *testing.T) {
	a, b, c := newReleaseID(), newReleaseID(), newReleaseID()
	if !(a < b && b < c) || !ValidReleaseID(a) {
		t.Fatalf("ids %s %s %s", a, b, c)
	}
}

// printer is an executor that writes fixed output.
type printer string

func (p printer) Run(_ context.Context, _ string, _ executor.Options, w io.Writer) error {
	_, err := io.WriteString(w, string(p))
	return err
}

func TestEnvForEnvironment(t *testing.T) {
	in := "APP_ENV=production\nAPP_URL=https://app.example.com\nASSET_URL=https://cdn.other.net\n" +
		"ALLOWED_HOSTS=app.example.com,www.app.example.com\nSESSION_DOMAIN=.app.example.com\n" +
		"DB_HOST=app.example.com\nREDIS_HOST=app.example.com\nMAIL_FROM_ADDRESS=hi@app.example.com\n" +
		"NEXT_PUBLIC_SITE_URL=\"https://app.example.com/\"\nOTHER_URL=https://myapp.example.com\n# APP_URL=https://app.example.com\n"
	out, changed := EnvForEnvironment(in, "staging", "app.example.com", "staging.app.example.com", "https")
	for _, want := range []string{
		"APP_ENV=staging\n", "APP_URL=https://staging.app.example.com\n", "ASSET_URL=https://cdn.other.net\n",
		"ALLOWED_HOSTS=staging.app.example.com,www.staging.app.example.com\n", "SESSION_DOMAIN=.staging.app.example.com\n",
		"DB_HOST=app.example.com\n", "REDIS_HOST=app.example.com\n", "MAIL_FROM_ADDRESS=hi@app.example.com\n",
		"NEXT_PUBLIC_SITE_URL=\"https://staging.app.example.com/\"\n", "OTHER_URL=https://myapp.example.com\n",
		"# APP_URL=https://app.example.com\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Join(changed, ",") != "APP_ENV,APP_URL,ALLOWED_HOSTS,SESSION_DOMAIN,NEXT_PUBLIC_SITE_URL" {
		t.Errorf("changed = %v", changed)
	}
	// APP_URL that names another host (or an IP) still gets the environment's.
	out, _ = EnvForEnvironment("APP_URL=http://203.0.113.7\n", "staging", "203.0.113.7", "staging.example.com", "http")
	if out != "APP_URL=http://staging.example.com\n" {
		t.Errorf("ip APP_URL: %q", out)
	}
}

func TestDeployCommit(t *testing.T) {
	f := &failAt{}
	sha := strings.Repeat("ab", 20)
	if err := (&Deployer{Exec: f}).DeployCommit(context.Background(), &bytes.Buffer{}, testSite, "", nil, sha); err != nil {
		t.Fatal(err)
	}
	if !f.ran("REF='"+sha+"'") || !f.ran(`rev-parse HEAD > "$REL/.skiff-commit"`) {
		t.Errorf("scripts: %v", f.scripts)
	}
	if err := (&Deployer{Exec: f}).DeployCommit(context.Background(), &bytes.Buffer{}, testSite, "", nil, "main; rm -rf /"); err == nil {
		t.Error("accepted a bad commit")
	}
}
