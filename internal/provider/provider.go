// Package provider defines the code-hosting provider abstraction used by
// mcp-forj: neutral data types, the Provider interface, a registry and a
// factory.
package provider

import (
	"context"
	"errors"
	"fmt"
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

// HTTPError carries an HTTP status code while wrapping a sentinel error. Its
// message is safe to surface (no response body).
type HTTPError struct {
	// Status is the HTTP status code.
	Status int
	// Err is the wrapped cause/sentinel.
	Err error
}

// Error implements error.
func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s (HTTP %d)", e.Err, e.Status)
}

// Unwrap returns the wrapped error.
func (e *HTTPError) Unwrap() error {
	return e.Err
}

// HTTPStatus returns the HTTP status carried by err, or 0 when none is present.
func HTTPStatus(err error) int {
	if httpErr, ok := errors.AsType[*HTTPError](err); ok {
		return httpErr.Status
	}
	return 0
}

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

// Commit is a provider-neutral commit.
type Commit struct {
	// SHA is the full commit object id.
	SHA string
	// ShortSHA is the provider's abbreviated commit id.
	ShortSHA string
	// Title is the first line of the commit message.
	Title string
	// Message is the full commit message.
	Message string
	// AuthorName is the commit author's display name.
	AuthorName string
	// AuthoredAt is the time the change was authored.
	AuthoredAt time.Time
	// CommittedAt is the time the commit was created (the date used for
	// filtering and sorting).
	CommittedAt time.Time
	// WebURL is the human-facing commit URL.
	WebURL string
}

// CommitAuthor identifies a commit author for exact, case-insensitive
// matching. When Email is set it is the matching key; otherwise Name is.
type CommitAuthor struct {
	// Email is the author email, the exact matching key when set.
	Email string
	// Name is the author display name, the exact matching key when Email is
	// empty.
	Name string
}

// CommitListOptions bounds and filters commit enumeration.
type CommitListOptions struct {
	// Since and Until bound the committer date, inclusive; zero means
	// unbounded.
	Since, Until time.Time
	// Author restricts to one exact author identity. The zero value imposes no
	// author restriction.
	Author CommitAuthor
	// AuthorQuery is a provider-side author filter (substring of name/email)
	// for an explicit author request; it is mutually exclusive with Author.
	AuthorQuery string
	// Limit caps the number of commits; zero means the provider default.
	Limit int
	// ScanLimit caps the number of commits inspected across pages, independent
	// of how many match the exact author filter; it bounds provider calls when
	// the server-side author prefilter over-matches (0 = provider default).
	ScanLimit int
}

// User is the authenticated identity at a provider.
type User struct {
	// Username is the provider login name.
	Username string
	// Name is the user's display name.
	Name string
	// Email is the user's email, empty when the token lacks the read_user
	// scope.
	Email string
}

// DiffFile is one file's diff within a merge request.
type DiffFile struct {
	// OldPath is the path before the change.
	OldPath string
	// NewPath is the path after the change.
	NewPath string
	// NewFile reports whether the file is newly added.
	NewFile bool
	// RenamedFile reports whether the file was renamed.
	RenamedFile bool
	// DeletedFile reports whether the file was deleted.
	DeletedFile bool
	// GeneratedFile reports whether the file is generated.
	GeneratedFile bool
	// TooLarge reports that the provider omitted the diff because it is too large.
	TooLarge bool
	// Diff is the unified diff text.
	Diff string
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
	// ListCommits lists commits of a repository's default branch, newest first,
	// bounded by opts. An exact Author match is applied by the provider (the
	// author email never leaves the provider layer). The bool reports that
	// enumeration stopped early (the Limit or ScanLimit was reached) and that
	// more commits may exist; it is false when the provider was naturally
	// exhausted.
	ListCommits(ctx context.Context, repo string, opts CommitListOptions) ([]Commit, bool, error)
	// CurrentUser returns the authenticated user's identity.
	CurrentUser(ctx context.Context) (User, error)
	// ListMergeRequests lists merge requests for a repository.
	ListMergeRequests(ctx context.Context, repo string, opts ListOptions) ([]MergeRequest, error)
	// GetMergeRequest fetches a single merge request by number.
	GetMergeRequest(ctx context.Context, repo string, number int64) (*MergeRequest, error)
	// ListMergeRequestNotes lists the notes of a merge request.
	ListMergeRequestNotes(ctx context.Context, repo string, number int64, opts ListOptions) ([]Note, error)
	// AddMergeRequestNote creates a note on a merge request.
	AddMergeRequestNote(ctx context.Context, repo string, number int64, body string) (*Note, error)
	// ListMergeRequestDiffs lists the file diffs of a merge request.
	ListMergeRequestDiffs(ctx context.Context, repo string, number int64) ([]DiffFile, error)
	// RebaseMergeRequest starts an asynchronous rebase of a merge request's
	// source branch onto its target branch.
	RebaseMergeRequest(ctx context.Context, repo string, number int64) error
	// MergeMergeRequest merges a merge request synchronously and returns the
	// merged merge request.
	MergeMergeRequest(ctx context.Context, repo string, number int64) (*MergeRequest, error)
	// UpdateMergeRequestLabels adds and/or removes labels on a merge request and
	// returns the updated merge request. At least one of add or remove must be
	// non-empty. Labels are provider content and must never be returned to
	// clients.
	UpdateMergeRequestLabels(ctx context.Context, repo string, number int64, add, remove []string) (*MergeRequest, error)

	// ReadFile reads a repository file. An empty ref means the default branch.
	ReadFile(ctx context.Context, repo, path, ref string) ([]byte, error)
	// FileExists reports whether a repository file exists. An empty ref means
	// the default branch.
	FileExists(ctx context.Context, repo, path, ref string) (bool, error)

	// GitAuthHeader returns the value of the Authorization header the git HTTP
	// transport must send to this provider for repo. The value carries a secret
	// and must never be logged, returned to clients, or embedded in URLs.
	GitAuthHeader(ctx context.Context, repo string) (string, error)
	// MergeBase returns the best common ancestor commit SHA of the given refs.
	MergeBase(ctx context.Context, repo string, refs ...string) (string, error)
	// GitRemoteURL returns the credential-free base clone URL of repo, ending in
	// ".git" (e.g. https://host/group/project.git). It must never contain a
	// token or any other credential.
	GitRemoteURL(ctx context.Context, repo string) (string, error)
	// DefaultBranch returns the repository's default branch name (e.g. "main").
	DefaultBranch(ctx context.Context, repo string) (string, error)
	// ResolveRef resolves a branch name to its commit SHA in repo. The git
	// proxy uses it for the fast-forward check, so only branch names (the
	// part after "refs/heads/") are part of the contract; tags and raw SHAs
	// need not resolve. It must return an error wrapping ErrNotFound when the
	// branch does not exist.
	ResolveRef(ctx context.Context, repo, branch string) (string, error)

	// CanonicalRepository returns the canonical authorization key for repo: the
	// exact repository identifier this provider resolves. The result is used for
	// BOTH policy matching and the provider call, so the two cannot diverge (a
	// mismatch would let a deny rule be bypassed). An implementation must reject
	// an identifier the backend would interpret differently from its namespaced
	// path (e.g. a bare numeric project id) and otherwise return a stable,
	// canonical value. Configured repository patterns must match that canonical
	// form. The caller passes an already structurally validated path.
	CanonicalRepository(repo string) (string, error)
}
