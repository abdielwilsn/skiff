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

type optRecorder struct {
	script string
	opt    executor.Options
	stdin  string
}

func (r *optRecorder) Run(_ context.Context, script string, opt executor.Options, _ io.Writer) error {
	r.script, r.opt = script, opt
	if opt.Stdin != nil {
		b, _ := io.ReadAll(opt.Stdin)
		r.stdin = string(b)
	}
	return nil
}

func TestRunCommand(t *testing.T) {
	rec := &optRecorder{}
	d := &Deployer{Exec: rec}
	site := store.Site{Domain: "app.example.com", Branch: "main", PHPVersion: "8.4", NodeVersion: "24"}
	var log bytes.Buffer
	if err := d.RunCommand(context.Background(), &log, site, "  php artisan migrate --force\nphp artisan db:seed --force  "); err != nil {
		t.Fatal(err)
	}
	if rec.opt.User != SiteUser {
		t.Errorf("runs as %q, want the site user", rec.opt.User)
	}
	if rec.opt.Stdin == nil || rec.stdin != "" {
		t.Error("stdin must be empty so prompts can't hang the job")
	}
	for _, want := range []string{
		"cd '/home/skiff/app.example.com'",
		"set -eo pipefail",
		"export PATH=/opt/skiff/php/8.4:$PATH",
		"export PATH=/opt/skiff/node/24/bin:$PATH",
		"php artisan migrate --force\nphp artisan db:seed --force",
	} {
		if !strings.Contains(rec.script, want) {
			t.Errorf("script missing %q:\n%s", want, rec.script)
		}
	}
	if !strings.HasPrefix(log.String(), "$ php artisan migrate --force\n$ php artisan db:seed --force\n") {
		t.Errorf("log should echo the command first, got %q", log.String())
	}
	if err := d.RunCommand(context.Background(), &log, site, "   "); err == nil {
		t.Error("empty command accepted")
	}
}
