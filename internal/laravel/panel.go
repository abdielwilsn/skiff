package laravel

import (
	"context"
	"fmt"
	"io"
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
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 120s;
    }
}
`

func PanelNginxConfig(domain, port string) string {
	return strings.NewReplacer("{{DOMAIN}}", domain, "{{PORT}}", port).Replace(panelNginxTemplate)
}

// EnablePanelHTTPS puts the panel behind Nginx at domain and gets it a
// certificate. port is the port the panel listens on locally.
func (d *Deployer) EnablePanelHTTPS(ctx context.Context, w io.Writer, domain, port, email string) error {
	if ok, why := HTTPSEligible(domain); !ok {
		return fmt.Errorf("%s", why)
	}
	if err := d.checkDNS(ctx, w, domain); err != nil {
		return err
	}

	step(w, "Configuring Nginx to proxy %s to the panel", domain)
	q := shell.Quote
	proxy := fmt.Sprintf(`cat > %[1]s
ln -sfn %[1]s %[2]s
nginx -t
service nginx reload`, q(PanelNginxConfigPath), q(panelNginxEnabled))
	if err := d.run(ctx, w, proxy, executor.Options{Stdin: strings.NewReader(PanelNginxConfig(domain, port))}); err != nil {
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
