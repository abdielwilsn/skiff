package laravel

import (
	"context"
	"fmt"
	"io"
	"strings"

	"skiff/internal/executor"
	"skiff/internal/shell"
	"skiff/internal/store"
)

// MaxCommandLen bounds what can be submitted from the Commands page.
const MaxCommandLen = 4000

// RunCommand runs an ad-hoc command (e.g. "php artisan migrate --force") in
// the site's directory as the site user, with the site's PHP and Node.js first
// on PATH, the same environment the deploy script gets. Commands can't prompt:
// stdin is empty, so confirmation questions get their default answer (no).
func (d *Deployer) RunCommand(ctx context.Context, w io.Writer, site store.Site, command string) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return fmt.Errorf("empty command")
	}
	dir := SitePath(site.Domain)
	for _, line := range strings.Split(command, "\n") {
		fmt.Fprintf(w, "$ %s\n", line)
	}
	fmt.Fprintln(w)
	env := []string{
		"SKIFF_SITE_PATH=" + dir,
		"SKIFF_BRANCH=" + site.Branch,
		"SKIFF_DOMAIN=" + site.Domain,
		"SKIFF_PHP=" + site.PHPVersion,
		"SKIFF_NODE=" + site.NodeVersion,
		"COMPOSER_NO_INTERACTION=1",
		"NPM_CONFIG_UPDATE_NOTIFIER=false",
	}
	script := fmt.Sprintf("cd %s || { echo \"The site hasn't been deployed yet.\" >&2; exit 1; }\nset -eo pipefail\n", shell.Quote(dir)) +
		withPHP(site.PHPVersion, withNode(site.NodeVersion, command))
	return d.Exec.Run(ctx, script, executor.Options{User: SiteUser, Env: env, Stdin: strings.NewReader("")}, w)
}
