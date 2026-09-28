package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestListReposPaginatesAndCaches(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/user/repos" {
			http.NotFound(w, r)
			return
		}
		page := r.URL.Query().Get("page")
		w.Header().Set("Content-Type", "application/json")
		if page == "" || page == "1" {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/user/repos?page=2&per_page=100>; rel="next", <http://%s/user/repos?page=2>; rel="last"`, r.Host, r.Host))
			fmt.Fprint(w, `[{"full_name":"me/app","clone_url":"https://github.com/me/app.git","ssh_url":"git@github.com:me/app.git","default_branch":"main","private":true,"pushed_at":"2026-01-01T00:00:00Z"}]`)
			return
		}
		fmt.Fprint(w, `[{"full_name":"org/lib","clone_url":"https://github.com/org/lib.git","default_branch":"master","archived":true}]`)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, TTL: time.Minute}
	repos, err := c.ListRepos(context.Background(), "tok", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos[0].FullName != "me/app" || !repos[0].Private || repos[1].DefaultBranch != "master" || !repos[1].Archived {
		t.Fatalf("repos = %+v", repos)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
	// Cached.
	if _, err := c.ListRepos(context.Background(), "tok", false); err != nil || calls.Load() != 2 {
		t.Fatalf("cache miss: calls=%d err=%v", calls.Load(), err)
	}
	// Refresh bypasses the cache.
	if _, err := c.ListRepos(context.Background(), "tok", true); err != nil || calls.Load() != 4 {
		t.Fatalf("refresh: calls=%d err=%v", calls.Load(), err)
	}
	// Bad token.
	if _, err := c.ListRepos(context.Background(), "nope", false); err != ErrUnauthorized {
		t.Fatalf("bad token err = %v", err)
	}
	if _, err := c.ListRepos(context.Background(), "", false); err != ErrUnauthorized {
		t.Fatalf("empty token err = %v", err)
	}
}

func TestNextLink(t *testing.T) {
	if got := nextLink(`<https://api.github.com/user/repos?page=3>; rel="next", <https://api.github.com/user/repos?page=9>; rel="last"`); got != "https://api.github.com/user/repos?page=3" {
		t.Fatal(got)
	}
	if got := nextLink(`<https://api.github.com/user/repos?page=1>; rel="prev"`); got != "" {
		t.Fatal(got)
	}
	if got := nextLink(""); got != "" {
		t.Fatal(got)
	}
}
