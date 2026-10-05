// Package gitlab implements the provider.Provider interface on top of the
// official GitLab API client.
package gitlab

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/hvo/mcp-forj/internal/config"
	"github.com/hvo/mcp-forj/internal/provider"
)

// providerType is the configuration type handled by this package.
const providerType = "gitlab"

func init() {
	provider.RegisterFactory(providerType, func(cfg config.ProviderConfig) (provider.Provider, error) {
		return New(cfg)
	})
}

// Client is a GitLab-backed provider.Provider.
type Client struct {
	name           string
	api            *gitlab.Client
	membershipOnly bool
}

// New creates a GitLab client from cfg. The token is read from cfg.Token,
// which has already been resolved by config.Parse, and is never stored in the
// configuration.
func New(cfg config.ProviderConfig) (*Client, error) {
	token := cfg.Token.Value()
	if token == "" {
		return nil, fmt.Errorf("gitlab: token is empty")
	}
	httpClient := &http.Client{Timeout: time.Duration(cfg.RequestTimeout)}
	api, err := gitlab.NewClient(token,
		gitlab.WithBaseURL(cfg.BaseURL),
		gitlab.WithHTTPClient(httpClient),
	)
	if err != nil {
		return nil, fmt.Errorf("gitlab: create client: %w", err)
	}
	return &Client{
		name:           cfg.Name,
		api:            api,
		membershipOnly: cfg.ProjectScope == "membership",
	}, nil
}

// Name implements provider.Provider.
func (c *Client) Name() string { return c.name }

// Type implements provider.Provider.
func (c *Client) Type() string { return providerType }

// ListRepositories implements provider.Provider. It lists projects visible to
// the token (all accessible projects by default, or only membership projects
// when project_scope is "membership"), following pages until the limit is
// reached or the provider is exhausted.
func (c *Client) ListRepositories(ctx context.Context, opts provider.RepoListOptions) ([]provider.Repository, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	perPage := limit
	if perPage > 100 {
		perPage = 100
	}
	membership := c.membershipOnly

	out := make([]provider.Repository, 0, limit)
	for page := int64(1); ; page++ {
		listOpts := &gitlab.ListProjectsOptions{
			ListOptions: gitlab.ListOptions{PerPage: int64(perPage), Page: page},
			Membership:  &membership,
		}
		if opts.Search != "" {
			search := opts.Search
			// GitLab searches path/name/description only; search_namespaces
			// additionally matches ancestor namespaces, so a full namespace
			// prefix such as "devops/platform" finds its projects (like the UI).
			includeNamespaces := true
			listOpts.Search = &search
			listOpts.SearchNamespaces = &includeNamespaces
		}
		projects, resp, err := c.api.Projects.ListProjects(listOpts, gitlab.WithContext(ctx))
		if err != nil {
			return nil, mapError(err)
		}
		for _, project := range projects {
			out = append(out, provider.Repository{
				Provider:    c.name,
				Path:        project.PathWithNamespace,
				WebURL:      project.WebURL,
				Topics:      append([]string(nil), project.Topics...),
				TopicsKnown: true,
			})
			if len(out) >= limit {
				return out, nil
			}
		}
		if len(projects) == 0 || resp == nil || resp.NextPage == 0 {
			return out, nil
		}
	}
}

// GetRepositoryTopics implements provider.Provider. It returns the project's
// topics, which the policy engine matches against repository tag filters.
func (c *Client) GetRepositoryTopics(ctx context.Context, repo string) ([]string, error) {
	project, _, err := c.api.Projects.GetProject(repo, nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, mapError(err)
	}
	return append([]string(nil), project.Topics...), nil
}

// ListMergeRequests implements provider.Provider.
func (c *Client) ListMergeRequests(ctx context.Context, repo string, opts provider.ListOptions) ([]provider.MergeRequest, error) {
	listOpts := &gitlab.ListProjectMergeRequestsOptions{}
	if opts.Limit > 0 {
		listOpts.PerPage = int64(opts.Limit)
	}
	if opts.State != "" {
		state := opts.State
		listOpts.State = &state
	}
	mrs, _, err := c.api.MergeRequests.ListProjectMergeRequests(repo, listOpts, gitlab.WithContext(ctx))
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]provider.MergeRequest, 0, len(mrs))
	for _, mr := range mrs {
		out = append(out, mapBasicMergeRequest(mr))
	}
	return out, nil
}

// GetMergeRequest implements provider.Provider.
func (c *Client) GetMergeRequest(ctx context.Context, repo string, number int64) (*provider.MergeRequest, error) {
	mr, _, err := c.api.MergeRequests.GetMergeRequest(repo, number, nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, mapError(err)
	}
	out := mapBasicMergeRequest(&mr.BasicMergeRequest)
	out.MergeError = mr.MergeError
	out.RebaseInProgress = mr.RebaseInProgress
	return &out, nil
}

// ListMergeRequestNotes implements provider.Provider.
func (c *Client) ListMergeRequestNotes(ctx context.Context, repo string, number int64, opts provider.ListOptions) ([]provider.Note, error) {
	listOpts := &gitlab.ListMergeRequestNotesOptions{}
	if opts.Limit > 0 {
		listOpts.PerPage = int64(opts.Limit)
	}
	notes, _, err := c.api.Notes.ListMergeRequestNotes(repo, number, listOpts, gitlab.WithContext(ctx))
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]provider.Note, 0, len(notes))
	for _, note := range notes {
		out = append(out, mapNote(note))
	}
	return out, nil
}

// AddMergeRequestNote implements provider.Provider.
func (c *Client) AddMergeRequestNote(ctx context.Context, repo string, number int64, body string) (*provider.Note, error) {
	note, _, err := c.api.Notes.CreateMergeRequestNote(repo, number, &gitlab.CreateMergeRequestNoteOptions{
		Body: &body,
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, mapError(err)
	}
	out := mapNote(note)
	return &out, nil
}

// RebaseMergeRequest implements provider.Provider. GitLab performs the rebase
// asynchronously (202 Accepted); the outcome is visible later on the merge
// request.
func (c *Client) RebaseMergeRequest(ctx context.Context, repo string, number int64) error {
	_, err := c.api.MergeRequests.RebaseMergeRequest(repo, number, nil, gitlab.WithContext(ctx))
	if err != nil {
		return mapError(err)
	}
	return nil
}

// ReadFile implements provider.Provider. An empty ref means the default branch.
func (c *Client) ReadFile(ctx context.Context, repo, path, ref string) ([]byte, error) {
	file, _, err := c.api.RepositoryFiles.GetFile(repo, path, refOptions(ref), gitlab.WithContext(ctx))
	if err != nil {
		return nil, mapError(err)
	}
	return decodeFile(file)
}

// FileExists implements provider.Provider. A missing file is not an error.
func (c *Client) FileExists(ctx context.Context, repo, path, ref string) (bool, error) {
	_, _, err := c.api.RepositoryFiles.GetFile(repo, path, refOptions(ref), gitlab.WithContext(ctx))
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, mapError(err)
	}
	return true, nil
}

func refOptions(ref string) *gitlab.GetFileOptions {
	if ref == "" {
		return nil
	}
	return &gitlab.GetFileOptions{Ref: &ref}
}

func decodeFile(file *gitlab.File) ([]byte, error) {
	if strings.EqualFold(file.Encoding, "base64") {
		cleaned := strings.NewReplacer("\n", "", "\r", "").Replace(file.Content)
		data, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			return nil, fmt.Errorf("gitlab: decode file content: %w", err)
		}
		return data, nil
	}
	return []byte(file.Content), nil
}

// mapBasicMergeRequest maps a GitLab merge request (list or detail) to the
// provider type. GitLab returns `labels` on both endpoints; LabelsKnown is true
// only when the field is present (JSON `[]` yields a non-nil empty slice, while
// `null`/absent yields nil), so unknown labels still fail closed.
func mapBasicMergeRequest(m *gitlab.BasicMergeRequest) provider.MergeRequest {
	out := provider.MergeRequest{
		Number:              m.IID,
		Title:               m.Title,
		Description:         m.Description,
		State:               m.State,
		SourceBranch:        m.SourceBranch,
		TargetBranch:        m.TargetBranch,
		WebURL:              m.WebURL,
		Labels:              append([]string(nil), m.Labels...),
		LabelsKnown:         m.Labels != nil,
		HasConflicts:        m.HasConflicts,
		DetailedMergeStatus: m.DetailedMergeStatus,
	}
	if m.Author != nil {
		out.Author = m.Author.Username
	}
	return out
}

func mapNote(n *gitlab.Note) provider.Note {
	out := provider.Note{
		ID:     n.ID,
		Body:   n.Body,
		Author: n.Author.Username,
	}
	if n.CreatedAt != nil {
		out.CreatedAt = *n.CreatedAt
	}
	return out
}

func isNotFound(err error) bool {
	return errors.Is(err, gitlab.ErrNotFound) || gitlab.HasStatusCode(err, http.StatusNotFound)
}

func mapError(err error) error {
	switch {
	case isNotFound(err):
		return provider.ErrNotFound
	case gitlab.HasStatusCode(err, http.StatusUnauthorized), gitlab.HasStatusCode(err, http.StatusForbidden):
		return provider.ErrForbidden
	case gitlab.HasStatusCode(err, http.StatusBadRequest),
		gitlab.HasStatusCode(err, http.StatusMethodNotAllowed),
		gitlab.HasStatusCode(err, http.StatusConflict):
		return provider.ErrInvalidState
	default:
		return errors.New("gitlab: request failed")
	}
}
