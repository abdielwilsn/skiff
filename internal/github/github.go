// Package github talks to GitHub with an OAuth app token: the OAuth web flow,
// listing repositories, and authenticating git without storing the token on disk.
package github

import (
	"context"
	"encoding/base64"
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

func get(ctx context.Context, token, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "skiff-panel")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
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
