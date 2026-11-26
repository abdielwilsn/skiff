package laravel

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"skiff/internal/store"
)

func TestPHPVersions(t *testing.T) {
	if !ValidPHP(DefaultPHP) {
		t.Fatalf("default PHP %s is not selectable", DefaultPHP)
	}
	for _, bad := range []string{"", "7.4", "8.4; rm -rf /", "8"} {
		if ValidPHP(bad) {
			t.Errorf("ValidPHP(%q) = true", bad)
		}
	}
	if got := withPHP("8.4", "php -v"); !strings.HasPrefix(got, "export PATH=/opt/skiff/php/8.4:$PATH\n") {
		t.Errorf("withPHP = %q", got)
	}
	if got := withPHP("bogus", "php -v"); got != "php -v" {
		t.Errorf("withPHP with an invalid version should not touch PATH: %q", got)
	}
}

func TestScriptsHaveNoLeftoverPlaceholders(t *testing.T) {
	for name, s := range map[string]string{
		"provision":  provisionScript(DefaultPHP),
		"ensure-php": ensurePHPScript("8.2"),
	} {
		if strings.Contains(s, "{{") {
			t.Errorf("%s script has an unreplaced placeholder", name)
		}
	}
	s := ensurePHPScript("8.2")
	for _, want := range []string{"php8.2-fpm", "/run/php/php8.2-fpm-skiff.sock", "/opt/skiff/php/8.2", "packages.sury.org/php/ ${code} main", "rm -f $OLD_PPA"} {
		if !strings.Contains(s, want) {
			t.Errorf("ensure-php script for 8.2 missing %q", want)
		}
	}
	// grep -q after apt-cache exits early; under pipefail that made a
	// version Ubuntu ships look missing.
	if strings.Contains(s, "| grep -q '^Version:'") {
		t.Error("ensure-php checks apt-cache with grep -q")
	}
}

func TestDeployUsesSitePHP(t *testing.T) {
	rec := &recorder{}
	d := &Deployer{Exec: rec}
	site := store.Site{Domain: "app.example.com", Repo: "o/r", Branch: "main", PHPVersion: "8.4", DeployScript: "composer install\n"}
	if err := d.Deploy(context.Background(), &bytes.Buffer{}, site, "", nil); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(rec.scripts, "\n----\n")
	for _, want := range []string{
		`PHP="8.4"`,                            // installs 8.4 if missing
		"export PATH=/opt/skiff/php/8.4:$PATH", // deploy script runs on 8.4
		"unix:/run/php/php8.4-fpm-skiff.sock",  // nginx socket follows the version
		"service php8.4-fpm reload",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("deploy scripts missing %q", want)
		}
	}
	if !strings.Contains(strings.Join(rec.stdin, "\n"), "fastcgi_pass unix:/run/php/php8.4-fpm-skiff.sock") {
		t.Error("new Nginx config should use the site's PHP socket")
	}
}

func TestNginxHeaderBuffers(t *testing.T) {
	cfg := NginxConfig(store.Site{Domain: "app.example.com", PHPVersion: "8.4"})
	for _, want := range []string{"fastcgi_buffer_size 64k;", "fastcgi_buffers 16 32k;"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("new site config missing %q (large Laravel headers would 502)", want)
		}
	}
	rec := &recorder{}
	d := &Deployer{Exec: rec}
	site := store.Site{Domain: "app.example.com", Repo: "o/r", Branch: "main", PHPVersion: "8.4", DeployScript: "true\n"}
	if err := d.Deploy(context.Background(), &bytes.Buffer{}, site, "", nil); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(rec.scripts, "\n")
	if !strings.Contains(all, "if ! grep -q 'fastcgi_buffer_size'") || !strings.Contains(all, `\1fastcgi_buffers 16 32k;`) {
		t.Error("deploys must add the buffers to existing site configs that lack them")
	}
}
