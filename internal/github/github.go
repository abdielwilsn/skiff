// Package github talks to GitHub with an OAuth app token: the OAuth web flow,
// listing repositories, and authenticating git without storing the token on disk.
package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Scopes requested from GitHub: "repo" to clone private repositories.
const Scopes = "repo read:user"

var ErrUnauthorized = errors.New("github token is invalid or was revoked")

var httpClient = &http.Client{Timeout: 20 * time.Second}

func AuthorizeURL(clientID, redirectURI, state string) string {
	v := url.Values{
		"client_id":    {clientID},
		"redirect_uri": {redirectURI},
		"scope":        {Scopes},
		"state":        {state},
	}
	return "https://github.com/login/oauth/authorize?" + v.Encode()
}

// Exchange trades an OAuth callback code for an access token.
func Exchange(ctx context.Context, clientID, clientSecret, code, redirectURI string) (string, error) {
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://github.com/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("github token exchange: %w", err)
	}
	if out.Error != "" {
		if out.Description != "" {
			return "", fmt.Errorf("github: %s", out.Description)
		}
		return "", fmt.Errorf("github: %s", out.Error)
	}
	if out.AccessToken == "" {
		return "", errors.New("github returned no access token")
	}
	return out.AccessToken, nil
}

// apiBase is the GitHub API root; tests point it at a fake server.
var apiBase = "https://api.github.com"

// ErrNotFound is returned for 404s, which GitHub also uses for "no access".
var ErrNotFound = errors.New("not found on GitHub (or no access)")

func call(ctx context.Context, method, token, path string, body, v any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "skiff-panel")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode/100 != 2:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if v == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func get(ctx context.Context, token, path string, v any) error {
	return call(ctx, http.MethodGet, token, path, nil, v)
}

type User struct {
	Login string `json:"login"`
	Name  string `json:"name"`
}

func CurrentUser(ctx context.Context, token string) (User, error) {
	var u User
	err := get(ctx, token, "/user", &u)
	return u, err
}

type Repo struct {
	FullName      string    `json:"full_name"`
	Description   string    `json:"description"`
	Private       bool      `json:"private"`
	Archived      bool      `json:"archived"`
	DefaultBranch string    `json:"default_branch"`
	Language      string    `json:"language"`
	HTMLURL       string    `json:"html_url"`
	PushedAt      time.Time `json:"pushed_at"`
}

// ListRepos returns repositories the user owns, collaborates on, or can access
// through an organization, most recently pushed first (up to 1000).
func ListRepos(ctx context.Context, token string) ([]Repo, error) {
	var all []Repo
	for page := 1; page <= 10; page++ {
		var batch []Repo
		path := fmt.Sprintf("/user/repos?per_page=100&sort=pushed&page=%d", page)
		if err := get(ctx, token, path, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return all, nil
}

func CloneURL(repo string) string { return "https://github.com/" + repo + ".git" }

// GitEnv returns environment variables that let git authenticate to GitHub
// over HTTPS with token, without writing it into .git/config or the remote URL.
// Requires git >= 2.31 (Ubuntu 22.04 ships 2.34).
func GitEnv(token string) []string {
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	if token == "" {
		return env
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return append(env,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+basic,
	)
}

// CreatePushHook adds a webhook to repo that POSTs push events to url, signed
// with secret. It returns GitHub's id for the hook. The OAuth "repo" scope
// covers repository webhooks; the user needs admin access to the repo.
func CreatePushHook(ctx context.Context, token, repo, url, secret string) (int64, error) {
	body := map[string]any{
		"name":   "web",
		"active": true,
		"events": []string{"push"},
		"config": map[string]string{
			"url":          url,
			"content_type": "json",
			"secret":       secret,
			"insecure_ssl": "0",
		},
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := call(ctx, http.MethodPost, token, "/repos/"+repo+"/hooks", body, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// DeleteHook removes a webhook. A hook that is already gone is not an error.
func DeleteHook(ctx context.Context, token, repo string, id int64) error {
	err := call(ctx, http.MethodDelete, token, fmt.Sprintf("/repos/%s/hooks/%d", repo, id), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ValidSignature checks GitHub's X-Hub-Signature-256 header ("sha256=<hex>")
// against an HMAC-SHA256 of the raw request body.
func ValidSignature(secret string, body []byte, header string) bool {
	if secret == "" || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}
