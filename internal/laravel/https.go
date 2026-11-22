package laravel

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"skiff/internal/executor"
	"skiff/internal/shell"
	"skiff/internal/store"
)

// HTTPS uses Let's Encrypt through certbot's Nginx plugin. certbot edits the
// site's existing server block in place (so user edits survive), adds an
// HTTP->HTTPS redirect, and Ubuntu's certbot package renews certificates
// automatically with a systemd timer.

func certPath(domain string) string { return "/etc/letsencrypt/live/" + domain + "/cert.pem" }

// reservedSuffixes can never get a publicly trusted certificate.
var reservedSuffixes = []string{".test", ".local", ".localhost", ".internal", ".invalid", ".example", ".lan", ".home.arpa"}

// HTTPSEligible reports whether Let's Encrypt can issue for domain, and why not.
func HTTPSEligible(domain string) (bool, string) {
	if net.ParseIP(domain) != nil {
		return false, "Let's Encrypt can't issue certificates for IP addresses. Use a domain name pointed at this server."
	}
	if domain == "localhost" || !strings.Contains(domain, ".") {
		return false, "HTTPS needs a public domain name (like app.example.com) pointed at this server."
	}
	for _, s := range reservedSuffixes {
		if strings.HasSuffix(domain, s) {
			return false, "Domains ending in " + s + " aren't public, so Let's Encrypt can't issue for them."
		}
	}
	return true, ""
}

func (d *Deployer) certbotFlags(domain, email string) string {
	args := []string{
		"--nginx", "--non-interactive", "--agree-tos",
		"--cert-name", domain, "-d", domain,
		"--redirect", "--keep-until-expiring",
	}
	if email != "" {
		args = append(args, "--email", email)
	} else {
		args = append(args, "--register-unsafely-without-email")
	}
	if d.ACMEServer != "" {
		args = append(args, "--server", d.ACMEServer)
	}
	if d.ACMEInsecure {
		args = append(args, "--no-verify-ssl")
	}
	for i, a := range args {
		args[i] = shell.Quote(a)
	}
	return strings.Join(args, " ")
}

const installCertbot = pkgsInstalledFn + `
if ! pkgs_installed certbot python3-certbot-nginx; then
    echo "Installing certbot (first time only)"
    export DEBIAN_FRONTEND=noninteractive
    dpkg --configure -a || true   # finish any interrupted install first
    apt-get update -q
    apt-get install -y -q certbot python3-certbot-nginx
fi
certbot --version`

// pkgsInstalledFn defines a bash function that succeeds only when every named
// package is fully installed. ("dpkg -s" also succeeds for half-configured
// packages, which would make a failed install look done and never be retried.)
const pkgsInstalledFn = `pkgs_installed() {
    for p in "$@"; do
        dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -q "install ok installed" || return 1
    done
}`

// EnableHTTPS obtains a certificate for the site and switches it to HTTPS.
func (d *Deployer) EnableHTTPS(ctx context.Context, w io.Writer, site store.Site, email string) error {
	if ok, why := HTTPSEligible(site.Domain); !ok {
		return fmt.Errorf("%s", why)
	}
	if err := d.checkDNS(ctx, w, site.Domain); err != nil {
		return err
	}
	if err := d.ensureCertbot(ctx, w); err != nil {
		return err
	}
	if err := d.issueCert(ctx, w, site.Domain, email, NginxConfigPath(site.Domain)); err != nil {
		return err
	}
	step(w, "Switching APP_URL to https")
	if err := d.setAppURLScheme(ctx, w, site, "http", "https"); err != nil {
		return err
	}
	fmt.Fprintf(w, "\nLive at https://%s (certificates renew automatically)\n", site.Domain)
	return nil
}

// checkDNS fails early, with an explanation, when domain doesn't resolve.
func (d *Deployer) checkDNS(ctx context.Context, w io.Writer, domain string) error {
	step(w, "Checking DNS for %s", domain)
	// getent exits non-zero for unknown names; "|| true" keeps pipefail from
	// aborting before the explanation below is printed.
	dns := fmt.Sprintf(`RESOLVED="$(getent ahosts %[1]s 2>/dev/null | awk '{print $1}' | sort -u | tr '\n' ' ' || true)"
if [ -z "$RESOLVED" ]; then
    echo "%[2]s does not resolve yet. Add a DNS A record pointing at this server, wait for it to propagate, then try again." >&2
    exit 1
fi
echo "%[2]s resolves to: $RESOLVED"
MINE="$(hostname -I 2>/dev/null || true)"
MATCH=no
for ip in $RESOLVED; do
    case " $MINE " in *" $ip "*) MATCH=yes ;; esac
done
if [ "$MATCH" = yes ]; then
    echo "That's this server."
else
    echo "Note: none of those addresses are on this server's interfaces ($MINE)."
    echo "That's expected behind NAT or a load balancer; otherwise the DNS record may point elsewhere."
fi`, shell.Quote(domain), domain)
	if err := d.run(ctx, w, dns, executor.Options{}); err != nil {
		return fmt.Errorf("DNS check: %w", err)
	}
	return nil
}

func (d *Deployer) ensureCertbot(ctx context.Context, w io.Writer) error {
	step(w, "Preparing certbot")
	if err := d.run(ctx, w, installCertbot, executor.Options{}); err != nil {
		return fmt.Errorf("install certbot: %w", err)
	}
	return nil
}

// issueCert runs certbot against the server block in confPath, which must
// already exist and contain server_name domain.
func (d *Deployer) issueCert(ctx context.Context, w io.Writer, domain, email, confPath string) error {
	step(w, "Requesting a Let's Encrypt certificate")
	env := ""
	if d.ACMEInsecure {
		env = "PYTHONWARNINGS=ignore " // silence per-request TLS warnings from test CAs
	}
	issue := fmt.Sprintf(`test -f %[1]s || { echo "Missing Nginx config %[1]s; deploy the site first." >&2; exit 1; }
%[3]scertbot %[2]s
nginx -t
service nginx reload`, shell.Quote(confPath), d.certbotFlags(domain, email), env)
	if err := d.run(ctx, w, issue, executor.Options{}); err != nil {
		fmt.Fprint(w, `
Let's Encrypt could not verify the domain. Common causes:
  - DNS doesn't point at this server yet (it can take a while to propagate)
  - port 80 is blocked by a firewall (ufw, or your provider's cloud firewall)
  - too many recent attempts for this domain (Let's Encrypt rate limits)
`)
		return fmt.Errorf("certbot: %w", err)
	}
	return nil
}

// DisableHTTPS removes the certificate and resets the site's Nginx config to
// Skiff's plain-HTTP template.
func (d *Deployer) DisableHTTPS(ctx context.Context, w io.Writer, site store.Site) error {
	q := shell.Quote
	step(w, "Restoring the plain HTTP Nginx config")
	ng := fmt.Sprintf(`cat > %[1]s
ln -sfn %[1]s %[2]s
nginx -t
service nginx reload`, q(NginxConfigPath(site.Domain)), q(nginxEnabledPath(site.Domain)))
	if err := d.run(ctx, w, ng, executor.Options{Stdin: strings.NewReader(NginxConfig(site))}); err != nil {
		return fmt.Errorf("restore nginx config: %w", err)
	}

	step(w, "Deleting the certificate")
	if err := d.run(ctx, w, d.deleteCertScript(site.Domain), executor.Options{}); err != nil {
		return err
	}

	step(w, "Switching APP_URL to http")
	return d.setAppURLScheme(ctx, w, site, "https", "http")
}

func (d *Deployer) deleteCertScript(domain string) string {
	return fmt.Sprintf(`if command -v certbot >/dev/null 2>&1 && [ -d /etc/letsencrypt/live/%[1]s ]; then
    certbot delete --non-interactive --cert-name %[1]s
else
    echo "No certificate to delete."
fi`, shell.Quote(domain))
}

// setAppURLScheme rewrites APP_URL only if it is exactly from://domain, so a
// custom APP_URL is left alone.
func (d *Deployer) setAppURLScheme(ctx context.Context, w io.Writer, site store.Site, from, to string) error {
	script := locate(site.Domain) + fmt.Sprintf(`[ -d "$SITE" ] || { echo "Site directory missing; skipping."; exit 0; }
if [ -f "$ENVF" ] && grep -q '^APP_URL=%[1]s://%[3]s/\?$' "$ENVF"; then
    sed -i 's|^APP_URL=%[1]s://%[3]s/\?$|APP_URL=%[2]s://%[4]s|' "$ENVF"
    echo "APP_URL=%[2]s://%[4]s"
    if [ -f "$APP/bootstrap/cache/config.php" ]; then cd "$APP" && php artisan config:cache; fi
else
    echo "APP_URL is customised or missing; left unchanged."
fi`, from, to, strings.ReplaceAll(site.Domain, ".", `\.`), site.Domain)
	return d.run(ctx, w, withPHP(site.PHPVersion, script), executor.Options{User: SiteUser})
}

// CertExpiry reads when the site's certificate expires.
func (d *Deployer) CertExpiry(ctx context.Context, site store.Site) (time.Time, error) {
	var out bytes.Buffer
	err := d.Exec.Run(ctx, "openssl x509 -enddate -noout -in "+shell.Quote(certPath(site.Domain)), executor.Options{}, &out)
	if err != nil {
		return time.Time{}, err
	}
	v := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out.String()), "notAfter="))
	return time.Parse("Jan _2 15:04:05 2006 MST", v)
}
