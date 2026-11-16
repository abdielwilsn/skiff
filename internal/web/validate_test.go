package web

import "testing"

func TestValidDomain(t *testing.T) {
	good := []string{"example.com", "app.example.co.uk", "localhost", "203.0.113.10", "my-app.dev"}
	bad := []string{"", "..", "-x.com", "x-.com", "Example.com", "a/b", "a b", "a..b", "../etc", "x.com;rm"}
	for _, d := range good {
		if !validDomain(d) {
			t.Errorf("validDomain(%q) = false, want true", d)
		}
	}
	for _, d := range bad {
		if validDomain(d) {
			t.Errorf("validDomain(%q) = true, want false", d)
		}
	}
}

func TestRepoAndBranch(t *testing.T) {
	for in, want := range map[string]string{
		"laravel/laravel":                        "laravel/laravel",
		"https://github.com/laravel/laravel.git": "laravel/laravel",
		"git@github.com:acme/shop.git":           "acme/shop",
		" https://github.com/acme/shop/ ":        "acme/shop",
	} {
		if got := normalizeRepo(in); got != want {
			t.Errorf("normalizeRepo(%q) = %q, want %q", in, got, want)
		}
	}
	for _, r := range []string{"acme/..", "acme", "-acme/x", "acme/x y", "a/b/c"} {
		if validRepo(r) {
			t.Errorf("validRepo(%q) = true, want false", r)
		}
	}
	for _, b := range []string{"main", "release/1.x", "feature_a-b"} {
		if !validBranch(b) {
			t.Errorf("validBranch(%q) = false, want true", b)
		}
	}
	for _, b := range []string{"-f", "a..b", "x.lock", "a//b", "a/", "a b", "$(id)"} {
		if validBranch(b) {
			t.Errorf("validBranch(%q) = true, want false", b)
		}
	}
}

func TestNormalizeText(t *testing.T) {
	if got := normalizeText("a\r\nb"); got != "a\nb\n" {
		t.Errorf("normalizeText = %q", got)
	}
}
