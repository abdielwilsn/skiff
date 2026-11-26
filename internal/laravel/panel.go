package laravel

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"skiff/internal/executor"
	"skiff/internal/shell"
)

// The Skiff panel itself can be served at its own domain over HTTPS: Nginx
// terminates TLS (certificate from Let's Encrypt via certbot) and proxies to
// the panel on localhost.

const (
	PanelNginxConfigPath = "/etc/nginx/sites-available/skiff-panel"
	panelNginxEnabled    = "/etc/nginx/sites-enabled/skiff-panel"
)

const panelNginxTemplate = `# Managed by Skiff: HTTPS reverse proxy for the Skiff panel.
server {
    listen 80;
    listen [::]:80;
    server_name {{DOMAIN}};

    client_max_body_size 4m;

    location / {
        proxy_pass http://127.0.0.1:{{PORT}};
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Skiff-Proxy {{SECRET}};
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 120s;
    }
}
`

// PanelNginxConfig is the panel's proxy config. secret is sent to the panel
// in X-Skiff-Proxy so it knows a local request came through this proxy; the
// file is root-only so sites on the server can't read it.
func PanelNginxConfig(domain, port, secret string) string {
	return strings.NewReplacer("{{DOMAIN}}", domain, "{{PORT}}", port, "{{SECRET}}", secret).Replace(panelNginxTemplate)
}

var proxySecretRe = regexp.MustCompile(`^[0-9a-f]{32,128}$`)

// SetPanelProxySecret adds (or replaces) the X-Skiff-Proxy header in an
// existing panel proxy config, keeping certbot's edits, and makes the file
// root-only. The secret is passed in the environment, never on a command line.
func (d *Deployer) SetPanelProxySecret(ctx context.Context, w io.Writer, secret string) error {
	if !proxySecretRe.MatchString(secret) {
		return fmt.Errorf("invalid proxy secret")
	}
	step(w, "Adding the proxy secret to the panel's Nginx config")
	script := fmt.Sprintf(`CONF=%s
[ -f "$CONF" ] || { echo "No panel proxy config; nothing to do."; exit 0; }
umask 077
awk '
    /proxy_set_header X-Skiff-Proxy/ { next }
    { print }
    /proxy_set_header X-Real-IP/ {
        match($0, /^[ \t]*/)
        printf "%%sproxy_set_header X-Skiff-Proxy %%s;\n", substr($0, 1, RLENGTH), ENVIRON["SKIFF_PROXY_SECRET"]
    }
' "$CONF" > "$CONF.skiff-tmp"
chmod 600 "$CONF.skiff-tmp"
mv -f "$CONF.skiff-tmp" "$CONF"
echo "Added to $(grep -c 'X-Skiff-Proxy' "$CONF") location(s)"
nginx -t
service nginx reload`, shell.Quote(PanelNginxConfigPath))
	return d.run(ctx, w, script, executor.Options{Env: []string{"SKIFF_PROXY_SECRET=" + secret}})
}

// EnablePanelHTTPS puts the panel behind Nginx at domain and gets it a
// certificate. port is the port the panel listens on locally.
func (d *Deployer) EnablePanelHTTPS(ctx context.Context, w io.Writer, domain, port, email, secret string) error {
	if ok, why := HTTPSEligible(domain); !ok {
		return fmt.Errorf("%s", why)
	}
	if err := d.checkDNS(ctx, w, domain); err != nil {
		return err
	}

	step(w, "Configuring Nginx to proxy %s to the panel", domain)
	q := shell.Quote
	proxy := fmt.Sprintf(`umask 077
cat > %[1]s
chmod 600 %[1]s
ln -sfn %[1]s %[2]s
nginx -t
service nginx reload`, q(PanelNginxConfigPath), q(panelNginxEnabled))
	if err := d.run(ctx, w, proxy, executor.Options{Stdin: strings.NewReader(PanelNginxConfig(domain, port, secret))}); err != nil {
		return fmt.Errorf("configure nginx: %w", err)
	}

	err := d.ensureCertbot(ctx, w)
	if err == nil {
		err = d.issueCert(ctx, w, domain, email, PanelNginxConfigPath)
	}
	if err != nil {
		// Don't leave the panel exposed over plain HTTP on port 80.
		step(w, "Rolling back the panel proxy")
		_ = d.run(ctx, w, d.removePanelProxyScript(), executor.Options{})
		return err
	}

	fmt.Fprintf(w, `
The panel is now at https://%[1]s
Direct visits to port %[2]s will redirect there. SSH tunnels to localhost:%[2]s keep working.
If you use GitHub, update your OAuth app's callback URL to:
  https://%[1]s/github/callback
`, domain, port)
	return nil
}

// DisablePanelHTTPS removes the panel's proxy config and certificate.
func (d *Deployer) DisablePanelHTTPS(ctx context.Context, w io.Writer, domain string) error {
	step(w, "Removing the panel's Nginx proxy")
	if err := d.run(ctx, w, d.removePanelProxyScript(), executor.Options{}); err != nil {
		return err
	}
	if domain != "" {
		step(w, "Deleting the certificate for %s", domain)
		if err := d.run(ctx, w, d.deleteCertScript(domain), executor.Options{}); err != nil {
			return err
		}
	}
	return nil
}

func (d *Deployer) removePanelProxyScript() string {
	return fmt.Sprintf(`rm -f %s %s
nginx -t
service nginx reload`, shell.Quote(panelNginxEnabled), shell.Quote(PanelNginxConfigPath))
}
