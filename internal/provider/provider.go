// Package provider defines the code-hosting provider abstraction used by
// mcp-forj: neutral data types, the Provider interface, a registry and a
// factory.
package provider

import (
	"context"
	"errors"
	"time"
)

// Provider sentinel errors. Callers should use errors.Is to distinguish the
// failure modes.
var (
	// ErrNotFound is returned when a requested resource does not exist.
	ErrNotFound = errors.New("not found")
	// ErrForbidden is returned when the token lacks the required permission
	// (HTTP 401/403).
	ErrForbidden = errors.New("forbidden")
	// ErrInvalidState is returned when the resource is not in a state that
	// permits the operation (HTTP 400/405/409).
	ErrInvalidState = errors.New("invalid state")
)

// Repository identifies a repository at a provider.
type Repository struct {
	// Provider is the logical provider name from the configuration.
	Provider string
	// Path is the canonical namespace/project path.
	Path string
	// WebURL is the human-facing repository URL.
	WebURL string
	// Topics are the tags attached to the repository. They are only meaningful
	// when TopicsKnown is true.
	Topics []string
	// TopicsKnown reports whether Topics could be determined. When false, any
	// active repository tag filter fails closed.
	TopicsKnown bool
}

// MergeRequest is a provider-neutral merge request.
type MergeRequest struct {
	// Number is the provider-native number (GitLab iid, GitHub PR number, ...).
	Number int64
	// Title is the merge request title.
	Title string
	// Description is the merge request description.
	Description string
	// State is the provider state, e.g. "opened" or "closed".
	State string
	// Author is the author's username.
	Author string
	// SourceBranch is the source branch name.
	SourceBranch string
	// TargetBranch is the target branch name.
	TargetBranch string
	// WebURL is the human-facing merge request URL.
	WebURL string
	// Labels are the tags attached to the merge request. They are only
	// meaningful when LabelsKnown is true.
	Labels []string
	// LabelsKnown reports whether Labels could be determined. When false, any
	// active tag filter fails closed.
	LabelsKnown bool
	// MergeError is a provider-reported merge/rebase error message, if any.
	MergeError string
	// RebaseInProgress reports whether a rebase is currently running.
	RebaseInProgress bool
	// HasConflicts reports whether the merge request has conflicts with its
	// target branch.
	HasConflicts bool
	// DetailedMergeStatus is the provider's detailed mergeability status.
	DetailedMergeStatus string
}

// Note is a provider-neutral comment on a merge request.
type Note struct {
	// ID is the provider-native note identifier.
	ID int64
	// Body is the note text.
	Body string
	// Author is the author's username.
	Author string
	// CreatedAt is the note creation time.
	CreatedAt time.Time
}

// RepoListOptions bounds and filters repository enumeration.
type RepoListOptions struct {
	// Search is an optional provider-side search term (usually a namespace
	// prefix).
	Search string
	// Limit is the maximum number of repositories to return; zero means the
	// provider default.
	Limit int
}

// ListOptions bounds and filters list operations.
type ListOptions struct {
	// State filters by merge request state; empty means no filter.
	State string
	// Limit is the maximum number of results; zero means the provider default.
	Limit int
}

// Provider is the abstraction every code hosting backend implements.
type Provider interface {
	// Name returns the logical provider name from the configuration.
	Name() string
	// Type returns the provider type, e.g. "gitlab".
	Type() string

	// ListRepositories enumerates repositories the token is a member of.
	ListRepositories(ctx context.Context, opts RepoListOptions) ([]Repository, error)
	// GetRepositoryTopics returns the topics (tags) of a repository.
	GetRepositoryTopics(ctx context.Context, repo string) ([]string, error)
	// ListMergeRequests lists merge requests for a repository.
	ListMergeRequests(ctx context.Context, repo string, opts ListOptions) ([]MergeRequest, error)
	// GetMergeRequest fetches a single merge request by number.
	GetMergeRequest(ctx context.Context, repo string, number int64) (*MergeRequest, error)
	// ListMergeRequestNotes lists the notes of a merge request.
	ListMergeRequestNotes(ctx context.Context, repo string, number int64, opts ListOptions) ([]Note, error)
	// AddMergeRequestNote creates a note on a merge request.
	AddMergeRequestNote(ctx context.Context, repo string, number int64, body string) (*Note, error)
	// RebaseMergeRequest starts an asynchronous rebase of a merge request's
	// source branch onto its target branch.
	RebaseMergeRequest(ctx context.Context, repo string, number int64) error

	// ReadFile reads a repository file. An empty ref means the default branch.
	ReadFile(ctx context.Context, repo, path, ref string) ([]byte, error)
	// FileExists reports whether a repository file exists. An empty ref means
	// the default branch.
	FileExists(ctx context.Context, repo, path, ref string) (bool, error)
}
