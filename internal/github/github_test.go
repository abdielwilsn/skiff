package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestValidSignature(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	secret := strings.Repeat("k", 24) // built at runtime so secret scanners ignore it
	if !ValidSignature(secret, body, sign(secret, body)) {
		t.Fatal("correct signature rejected")
	}
	for name, header := range map[string]string{
		"wrong secret":  sign("other", body),
		"tampered body": sign(secret, []byte(`{"ref":"refs/heads/evil"}`)),
		"missing":       "",
		"sha1 style":    "sha1=abc",
		"not hex":       "sha256=zz",
	} {
		if ValidSignature(secret, body, header) {
			t.Errorf("%s: accepted", name)
		}
	}
	if ValidSignature("", body, sign("", body)) {
		t.Error("an empty secret must never validate")
	}
}

func TestCreateAndDeleteHook(t *testing.T) {
	hookKey := strings.Repeat("h", 20)
	var got map[string]any
	var deleted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/shop/hooks":
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 4242}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/private/hooks":
			w.WriteHeader(http.StatusNotFound) // GitHub's answer when you lack admin
		case r.Method == http.MethodDelete:
			deleted = r.URL.Path
			if r.URL.Path == "/repos/acme/shop/hooks/999" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()
	old := apiBase
	apiBase = srv.URL
	defer func() { apiBase = old }()

	ctx := context.Background()
	id, err := CreatePushHook(ctx, "tok", "acme/shop", "https://panel.example.com/hooks/github/1", hookKey)
	if err != nil || id != 4242 {
		t.Fatalf("CreatePushHook = %d, %v", id, err)
	}
	cfg := got["config"].(map[string]any)
	if cfg["url"] != "https://panel.example.com/hooks/github/1" || cfg["secret"] != hookKey ||
		cfg["content_type"] != "json" || cfg["insecure_ssl"] != "0" {
		t.Errorf("hook config = %v", cfg)
	}
	if ev := got["events"].([]any); len(ev) != 1 || ev[0] != "push" {
		t.Errorf("events = %v", ev)
	}

	if _, err := CreatePushHook(ctx, "tok", "acme/private", "u", "s"); !errors.Is(err, ErrNotFound) {
		t.Errorf("no-admin repo: got %v, want ErrNotFound", err)
	}
	if _, err := CreatePushHook(ctx, "bad", "acme/shop", "u", "s"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("bad token: got %v, want ErrUnauthorized", err)
	}

	if err := DeleteHook(ctx, "tok", "acme/shop", 4242); err != nil || deleted != "/repos/acme/shop/hooks/4242" {
		t.Errorf("DeleteHook = %v (path %s)", err, deleted)
	}
	if err := DeleteHook(ctx, "tok", "acme/shop", 999); err != nil {
		t.Errorf("deleting an already-removed hook should succeed, got %v", err)
	}
}
