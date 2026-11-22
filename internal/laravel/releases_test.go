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
	out := "noise line\n20261122141530\t1\tabc1234 feat: staff portal\n20261121093012\t0\tdef5678 fix\nbad\t1\tx\n"
	d := &Deployer{Exec: printer(out)}
	list, err := d.Releases(context.Background(), testSite)
	if err != nil || len(list) != 2 {
		t.Fatalf("Releases = %+v, %v", list, err)
	}
	if !list[0].Current || list[0].Revision != "abc1234 feat: staff portal" || list[1].Current || list[0].Built.IsZero() {
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
