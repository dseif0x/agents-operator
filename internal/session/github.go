package session

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/dseif0x/agents-operator/internal/github"
	"github.com/dseif0x/agents-operator/internal/store"
)

// GitHubRepo is one entry of the repository picker: a GitHub repository the
// user's token can see, plus how often this user has started a session
// with it.
type GitHubRepo struct {
	github.Repo
	Uses int `json:"uses"`
}

// GitHubRepos lists the repositories the user's GitHub token can access,
// sorted by how often the user has used them (desc) and then by name.
// Configured is false when the user has no GitHub token.
func (s *Service) GitHubRepos(ctx context.Context, userID string, refresh bool) (repos []GitHubRepo, configured bool, err error) {
	if s.GitHub == nil || s.Creds == nil {
		return nil, false, nil
	}
	token, err := s.Creds.Get(ctx, userID, store.CredGitHubToken)
	if err != nil {
		return nil, false, err
	}
	if len(token) == 0 {
		return nil, false, nil
	}
	list, err := s.GitHub.ListRepos(ctx, strings.TrimSpace(string(token)), refresh)
	if err != nil {
		if errors.Is(err, github.ErrUnauthorized) {
			return nil, true, &ValidationError{"GitHub rejected the configured token; replace it on the account page"}
		}
		return nil, true, err
	}
	usage, err := s.Store.RepoUsage().List(ctx, userID)
	if err != nil {
		return nil, true, err
	}
	repos = make([]GitHubRepo, 0, len(list))
	for _, r := range list {
		repos = append(repos, GitHubRepo{Repo: r, Uses: usage[store.RepoKey(r.CloneURL)]})
	}
	sort.SliceStable(repos, func(i, j int) bool {
		if repos[i].Uses != repos[j].Uses {
			return repos[i].Uses > repos[j].Uses
		}
		return strings.ToLower(repos[i].FullName) < strings.ToLower(repos[j].FullName)
	})
	return repos, true, nil
}
