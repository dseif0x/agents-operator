// Package github is the hub's minimal GitHub API client: it lists the
// repositories a token can see, for the "browse repositories" picker. It
// is the only place the hub talks to GitHub; agents use gh in their pods.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Repo is one repository as the picker needs it.
type Repo struct {
	FullName      string    `json:"full_name"`
	CloneURL      string    `json:"clone_url"`
	SSHURL        string    `json:"ssh_url"`
	DefaultBranch string    `json:"default_branch"`
	Private       bool      `json:"private"`
	Archived      bool      `json:"archived"`
	Description   string    `json:"description"`
	PushedAt      time.Time `json:"pushed_at"`
}

// ErrUnauthorized is returned when GitHub rejects the token.
var ErrUnauthorized = errors.New("github rejected the token")

// Client lists repositories with a small per-token cache, so the picker
// does not hit GitHub on every keystroke or page load.
type Client struct {
	BaseURL string // defaults to https://api.github.com
	HTTP    *http.Client
	// MaxPages caps pagination (100 repos per page).
	MaxPages int
	TTL      time.Duration

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	repos   []Repo
	fetched time.Time
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "https://api.github.com"
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// ListRepos returns every repository the token can access (owned,
// collaborator and organization repositories), cached per token for TTL
// unless refresh is set.
func (c *Client) ListRepos(ctx context.Context, token string, refresh bool) ([]Repo, error) {
	if token == "" {
		return nil, ErrUnauthorized
	}
	ttl := c.TTL
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	key := cacheKey(token)
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && !refresh && time.Since(e.fetched) < ttl {
		c.mu.Unlock()
		return e.repos, nil
	}
	c.mu.Unlock()

	repos, err := c.fetchAll(ctx, token)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]cacheEntry{}
	}
	c.cache[key] = cacheEntry{repos: repos, fetched: time.Now()}
	c.mu.Unlock()
	return repos, nil
}

func (c *Client) fetchAll(ctx context.Context, token string) ([]Repo, error) {
	maxPages := c.MaxPages
	if maxPages <= 0 {
		maxPages = 10 // 1000 repositories
	}
	var out []Repo
	next := c.base() + "/user/repos?" + url.Values{
		"per_page":    {"100"},
		"affiliation": {"owner,collaborator,organization_member"},
		"sort":        {"pushed"},
	}.Encode()
	for page := 0; next != "" && page < maxPages; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "agents-operator")
		resp, err := c.httpClient().Do(req)
		if err != nil {
			return nil, fmt.Errorf("github: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return nil, ErrUnauthorized
		case resp.StatusCode != http.StatusOK:
			return nil, fmt.Errorf("github: http %d", resp.StatusCode)
		}
		var page []struct {
			FullName      string    `json:"full_name"`
			CloneURL      string    `json:"clone_url"`
			SSHURL        string    `json:"ssh_url"`
			DefaultBranch string    `json:"default_branch"`
			Private       bool      `json:"private"`
			Archived      bool      `json:"archived"`
			Description   string    `json:"description"`
			PushedAt      time.Time `json:"pushed_at"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("github: bad response: %w", err)
		}
		for _, r := range page {
			out = append(out, Repo(r))
		}
		next = nextLink(resp.Header.Get("Link"))
	}
	return out, nil
}

// nextLink extracts rel="next" from a GitHub Link header.
func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		seg := strings.Split(strings.TrimSpace(part), ";")
		if len(seg) < 2 {
			continue
		}
		u := strings.Trim(strings.TrimSpace(seg[0]), "<>")
		for _, p := range seg[1:] {
			if strings.TrimSpace(p) == `rel="next"` {
				return u
			}
		}
	}
	return ""
}

// cacheKey avoids keeping raw tokens as map keys in memory dumps.
func cacheKey(token string) string {
	if len(token) <= 8 {
		return token
	}
	return token[:4] + "…" + token[len(token)-4:] + fmt.Sprintf("/%d", len(token))
}
