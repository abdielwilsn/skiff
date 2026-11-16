package laravel

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"skiff/internal/store"
)

func TestNodeVersions(t *testing.T) {
	if !ValidNode(DefaultNode) || !ValidNode("") {
		t.Fatal("default and 'none' must be valid")
	}
	for _, bad := range []string{"18", "24.1", "latest", "24; id"} {
		if ValidNode(bad) {
			t.Errorf("ValidNode(%q) = true", bad)
		}
	}
	if got := withNode("24", "npm -v"); !strings.HasPrefix(got, "export PATH=/opt/skiff/node/24/bin:$PATH\n") {
		t.Errorf("withNode = %q", got)
	}
	if got := withNode("", "npm -v"); got != "npm -v" {
		t.Errorf("withNode(none) changed the script: %q", got)
	}
}

func TestDeployBuildsAssetsWithSiteNode(t *testing.T) {
	rec := &recorder{}
	d := &Deployer{Exec: rec}
	site := store.Site{Domain: "app.example.com", Repo: "o/r", Branch: "main", PHPVersion: "8.4", NodeVersion: "24", DeployScript: DefaultDeployScript}
	if err := d.Deploy(context.Background(), &bytes.Buffer{}, site, "", nil); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(rec.scripts, "\n----\n")
	for _, want := range []string{
		`MAJOR="24"`, // installs Node 24 on demand
		"https://nodejs.org/dist/latest-v${MAJOR}.x",
		"sha256sum -c", // and verifies it
		"export PATH=/opt/skiff/node/24/bin:$PATH", // deploy script sees node/npm
		"--no-same-owner",                          // Node is root-owned, not owned by the archive uid
		`chown -R root:root "$DIR.tmp"`,
		"npm run build", // default script builds assets
	} {
		if !strings.Contains(all, want) {
			t.Errorf("deploy scripts missing %q", want)
		}
	}
	// Composer must run before the asset build (Vite plugins like Wayfinder
	// call php artisan), and the build before optimize.
	ds := DefaultDeployScript
	if !(strings.Index(ds, "composer install") < strings.Index(ds, "npm run build") &&
		strings.Index(ds, "npm run build") < strings.Index(ds, "php artisan optimize")) {
		t.Error("default deploy script steps are out of order")
	}
}
