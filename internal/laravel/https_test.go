package laravel

import (
	"strings"
	"testing"
)

func TestHTTPSEligible(t *testing.T) {
	for _, d := range []string{"example.com", "app.example.co.uk", "shop.dev"} {
		if ok, why := HTTPSEligible(d); !ok {
			t.Errorf("HTTPSEligible(%q) = false (%s), want true", d, why)
		}
	}
	for _, d := range []string{"localhost", "203.0.113.10", "myapp", "app.test", "nas.local", "x.home.arpa"} {
		if ok, _ := HTTPSEligible(d); ok {
			t.Errorf("HTTPSEligible(%q) = true, want false", d)
		}
	}
}

func TestCertbotFlags(t *testing.T) {
	d := &Deployer{}
	f := d.certbotFlags("app.example.com", "")
	for _, want := range []string{"'--nginx'", "'--redirect'", "'--cert-name' 'app.example.com'", "'--register-unsafely-without-email'"} {
		if !strings.Contains(f, want) {
			t.Errorf("flags %q missing %q", f, want)
		}
	}
	if strings.Contains(f, "--server") || strings.Contains(f, "--no-verify-ssl") {
		t.Errorf("production flags should not override the ACME server: %q", f)
	}
	d = &Deployer{ACMEServer: "https://acme.test/dir", ACMEInsecure: true}
	f = d.certbotFlags("app.example.com", "me@example.com")
	for _, want := range []string{"'--email' 'me@example.com'", "'--server' 'https://acme.test/dir'", "'--no-verify-ssl'"} {
		if !strings.Contains(f, want) {
			t.Errorf("flags %q missing %q", f, want)
		}
	}
}
