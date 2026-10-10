package gitlab

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/hvo/mcp-forj/internal/config"
	"github.com/hvo/mcp-forj/internal/provider"
)

func TestMapBasicMergeRequest(t *testing.T) {
	m := &gitlab.BasicMergeRequest{
		IID:          42,
		Title:        "Add feature",
		Description:  "body",
		State:        "opened",
		SourceBranch: "feat",
		TargetBranch: "main",
		WebURL:       "https://gitlab.example.com/team/app/-/merge_requests/42",
		Author:       &gitlab.BasicUser{Username: "alice"},
	}
	got := mapBasicMergeRequest(m)
	want := provider.MergeRequest{
		Number:       42,
		Title:        "Add feature",
		Description:  "body",
		State:        "opened",
		Author:       "alice",
		SourceBranch: "feat",
		TargetBranch: "main",
		WebURL:       "https://gitlab.example.com/team/app/-/merge_requests/42",
	}
	if got.LabelsKnown {
		t.Error("mapBasicMergeRequest set LabelsKnown, want false")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mapBasicMergeRequest = %+v, want %+v", got, want)
	}
}

func TestMapNote(t *testing.T) {
	created := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	n := &gitlab.Note{
		ID:        7,
		Body:      "looks good",
		Author:    gitlab.NoteAuthor{Username: "bob"},
		CreatedAt: &created,
	}
	got := mapNote(n)
	want := provider.Note{ID: 7, Body: "looks good", Author: "bob", CreatedAt: created}
	if got != want {
		t.Errorf("mapNote = %+v, want %+v", got, want)
	}
}

func TestMapNoteMissingAuthor(t *testing.T) {
	got := mapNote(&gitlab.Note{ID: 1, Body: "x"})
	if got.Author != "" {
		t.Errorf("Author = %q, want empty for a note without an author", got.Author)
	}
}

func TestNewMissingToken(t *testing.T) {
	_, err := New(config.ProviderConfig{
		Name:    "p",
		Type:    "gitlab",
		BaseURL: "https://gitlab.example.com",
	})
	if err == nil {
		t.Fatal("New succeeded with an empty token, want error")
	}
	if !strings.Contains(err.Error(), "token is empty") {
		t.Errorf("error = %q, want it to mention the empty token", err)
	}
}

func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(config.ProviderConfig{
		Name:           "p",
		Type:           "gitlab",
		BaseURL:        srv.URL,
		Token:          "test-token",
		RequestTimeout: config.Duration(5 * time.Second),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestListRepositoriesPagination(t *testing.T) {
	var pages, searches []string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/projects" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("membership"); got != "false" {
			t.Errorf("membership = %q, want false (default accessible scope)", got)
		}
		searches = append(searches, r.URL.Query().Get("search"))
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case "1":
			w.Header().Set("X-Next-Page", "2")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "path_with_namespace": "archive/a", "web_url": "https://x/archive/a"},
				{"id": 2, "path_with_namespace": "archive/b", "web_url": "https://x/archive/b"},
			})
		default:
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 3, "path_with_namespace": "archive/c", "web_url": "https://x/archive/c"},
			})
		}
	}))

	repos, err := c.ListRepositories(context.Background(), provider.RepoListOptions{Search: "archive", Limit: 3})
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(repos) != 3 {
		t.Fatalf("len = %d, want 3", len(repos))
	}
	if repos[2].Path != "archive/c" || repos[2].Provider != "p" {
		t.Errorf("third repo = %+v, want archive/c for provider p", repos[2])
	}
	if len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Errorf("pages = %v, want [1 2]", pages)
	}
	if len(searches) == 0 || searches[0] != "archive" {
		t.Errorf("searches = %v, want first = archive", searches)
	}

	// The limit stops collection before the second page.
	pages = nil
	repos, err = c.ListRepositories(context.Background(), provider.RepoListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("len = %d, want 2", len(repos))
	}
	if len(pages) != 1 {
		t.Errorf("pages = %v, want [1]", pages)
	}
}

func TestGetMergeRequestHTTP(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/merge_requests/42") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"iid":                   42,
			"title":                 "Add feature",
			"state":                 "opened",
			"source_branch":         "feat",
			"target_branch":         "main",
			"web_url":               "https://example.com/mr/42",
			"author":                map[string]any{"username": "alice"},
			"labels":                []string{"ai-reviewed", "backend"},
			"merge_error":           "rebase failed",
			"rebase_in_progress":    true,
			"has_conflicts":         true,
			"detailed_merge_status": "conflict",
		})
	}))

	mr, err := c.GetMergeRequest(context.Background(), "team/app", 42)
	if err != nil {
		t.Fatalf("GetMergeRequest: %v", err)
	}
	if mr.Number != 42 || mr.Author != "alice" || mr.Title != "Add feature" {
		t.Errorf("unexpected merge request: %+v", mr)
	}
	if !mr.LabelsKnown {
		t.Error("LabelsKnown = false, want true for GetMergeRequest")
	}
	if !reflect.DeepEqual(mr.Labels, []string{"ai-reviewed", "backend"}) {
		t.Errorf("Labels = %v, want [ai-reviewed backend]", mr.Labels)
	}
	if mr.MergeError != "rebase failed" || !mr.RebaseInProgress || !mr.HasConflicts || mr.DetailedMergeStatus != "conflict" {
		t.Errorf("rebase/merge status not mapped: %+v", mr)
	}
}

func TestMapErrorStatusMapping(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   error
	}{
		{"401 unauthorized", http.StatusUnauthorized, provider.ErrForbidden},
		{"403 forbidden", http.StatusForbidden, provider.ErrForbidden},
		{"400 bad request", http.StatusBadRequest, provider.ErrInvalidState},
		{"405 method not allowed", http.StatusMethodNotAllowed, provider.ErrInvalidState},
		{"409 conflict", http.StatusConflict, provider.ErrInvalidState},
		{"404 not found", http.StatusNotFound, provider.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &gitlab.ErrorResponse{Response: &http.Response{StatusCode: tt.status}}
			got := mapError(err)
			if !errors.Is(got, tt.want) {
				t.Errorf("mapError(%d) = %v, want %v", tt.status, got, tt.want)
			}
			if status := provider.HTTPStatus(got); status != tt.status {
				t.Errorf("HTTPStatus(mapError(%d)) = %d, want %d", tt.status, status, tt.status)
			}
		})
	}
	t.Run("500 generic", func(t *testing.T) {
		err := &gitlab.ErrorResponse{Response: &http.Response{StatusCode: http.StatusInternalServerError}}
		got := mapError(err)
		for _, sentinel := range []error{provider.ErrNotFound, provider.ErrForbidden, provider.ErrInvalidState} {
			if errors.Is(got, sentinel) {
				t.Errorf("mapError(500) = %v, unexpectedly matches %v", got, sentinel)
			}
		}
		if status := provider.HTTPStatus(got); status != http.StatusInternalServerError {
			t.Errorf("HTTPStatus(mapError(500)) = %d, want 500", status)
		}
		if !strings.Contains(got.Error(), "HTTP 500") {
			t.Errorf("error = %q, want it to include the status code", got)
		}
	})
	t.Run("no status", func(t *testing.T) {
		got := mapError(errors.New("dial tcp: connection refused"))
		if status := provider.HTTPStatus(got); status != 0 {
			t.Errorf("HTTPStatus = %d, want 0", status)
		}
	})
}

func TestGetMergeRequestNotFoundHTTP(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	_, err := c.GetMergeRequest(context.Background(), "team/app", 1)
	if !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("error = %v, want provider.ErrNotFound", err)
	}
	if status := provider.HTTPStatus(err); status != http.StatusNotFound {
		t.Errorf("HTTPStatus = %d, want 404", status)
	}
}

func TestFileExistsHTTP(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("ref"); got != "HEAD" {
			t.Errorf("file request ref = %q, want HEAD for default branch", got)
		}
		if strings.Contains(r.URL.Path, ".noai") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"file_name": "README.md",
			"encoding":  "base64",
			"content":   "aGVsbG8=",
		})
	}))

	exists, err := c.FileExists(context.Background(), "team/app", "README.md", "")
	if err != nil {
		t.Fatalf("FileExists: %v", err)
	}
	if !exists {
		t.Error("FileExists = false, want true")
	}

	exists, err = c.FileExists(context.Background(), "team/app", ".noai", "")
	if err != nil {
		t.Fatalf("FileExists(.noai): %v", err)
	}
	if exists {
		t.Error("FileExists(.noai) = true, want false")
	}
}

func TestReadFileDecodesBase64(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("ref"); got != "main" {
			t.Errorf("file request ref = %q, want explicit main", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"file_name": "README.md",
			"encoding":  "base64",
			"content":   "aGVsbG8gd29ybGQ=",
		})
	}))

	data, err := c.ReadFile(context.Background(), "team/app", "README.md", "main")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("ReadFile = %q, want hello world", data)
	}
}

func TestReadFilePlainText(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"file_name": "README.md",
			"encoding":  "text",
			"content":   "plain",
		})
	}))

	data, err := c.ReadFile(context.Background(), "team/app", "README.md", "")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "plain" {
		t.Errorf("ReadFile = %q, want plain", data)
	}
}

func TestListRepositoriesMapsTopics(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "path_with_namespace": "team/app", "web_url": "https://x/team/app", "topics": []string{"ai-ok", "backend"}},
			{"id": 2, "path_with_namespace": "team/plain", "web_url": "https://x/team/plain"},
		})
	}))

	repos, err := c.ListRepositories(context.Background(), provider.RepoListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("len = %d, want 2", len(repos))
	}
	if !repos[0].TopicsKnown || !reflect.DeepEqual(repos[0].Topics, []string{"ai-ok", "backend"}) {
		t.Errorf("repo[0] topics = %v (known=%v)", repos[0].Topics, repos[0].TopicsKnown)
	}
	if !repos[1].TopicsKnown || len(repos[1].Topics) != 0 {
		t.Errorf("repo[1] topics = %v (known=%v), want empty known set", repos[1].Topics, repos[1].TopicsKnown)
	}
}

func TestGetRepositoryTopicsHTTP(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/projects/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":                  1,
			"path_with_namespace": "team/app",
			"web_url":             "https://x/team/app",
			"topics":              []string{"ai-ok", "backend"},
		})
	}))

	topics, err := c.GetRepositoryTopics(context.Background(), "team/app")
	if err != nil {
		t.Fatalf("GetRepositoryTopics: %v", err)
	}
	if !reflect.DeepEqual(topics, []string{"ai-ok", "backend"}) {
		t.Errorf("topics = %v, want [ai-ok backend]", topics)
	}
}

func TestGetRepositoryTopicsNotFound(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	_, err := c.GetRepositoryTopics(context.Background(), "team/app")
	if !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("error = %v, want provider.ErrNotFound", err)
	}
	if status := provider.HTTPStatus(err); status != http.StatusNotFound {
		t.Errorf("HTTPStatus = %d, want 404", status)
	}
}

func TestRebaseMergeRequestHTTP(t *testing.T) {
	var gotMethod, gotPath string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))

	if err := c.RebaseMergeRequest(context.Background(), "team/app", 42); err != nil {
		t.Fatalf("RebaseMergeRequest: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %s, want PUT", gotMethod)
	}
	if !strings.HasSuffix(gotPath, "/merge_requests/42/rebase") {
		t.Errorf("path = %s, want suffix /merge_requests/42/rebase", gotPath)
	}
}

func TestRebaseMergeRequestHTTPError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	err := c.RebaseMergeRequest(context.Background(), "team/app", 42)
	if err == nil {
		t.Fatal("RebaseMergeRequest succeeded on 403, want error")
	}
	if !errors.Is(err, provider.ErrForbidden) {
		t.Fatalf("error = %v, want provider.ErrForbidden", err)
	}
	if got := provider.HTTPStatus(err); got != http.StatusForbidden {
		t.Errorf("HTTPStatus = %d, want 403", got)
	}
}

func TestMergeMergeRequestHTTP(t *testing.T) {
	var gotMethod, gotPath string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"iid": 42, "title": "merge me", "state": "merged",
		})
	}))

	mr, err := c.MergeMergeRequest(context.Background(), "team/app", 42)
	if err != nil {
		t.Fatalf("MergeMergeRequest: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %s, want PUT", gotMethod)
	}
	if !strings.HasSuffix(gotPath, "/merge_requests/42/merge") {
		t.Errorf("path = %s, want suffix /merge_requests/42/merge", gotPath)
	}
	if mr.State != "merged" {
		t.Errorf("state = %q, want merged", mr.State)
	}
}

func TestMergeMergeRequestHTTPInvalidState(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	_, err := c.MergeMergeRequest(context.Background(), "team/app", 42)
	if err == nil {
		t.Fatal("MergeMergeRequest succeeded on 405, want error")
	}
	if !errors.Is(err, provider.ErrInvalidState) {
		t.Fatalf("error = %v, want provider.ErrInvalidState", err)
	}
	if got := provider.HTTPStatus(err); got != http.StatusMethodNotAllowed {
		t.Errorf("HTTPStatus = %d, want 405", got)
	}
}

func TestMergeMergeRequestHTTPNotAcceptable(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotAcceptable)
	}))
	_, err := c.MergeMergeRequest(context.Background(), "team/app", 42)
	if err == nil {
		t.Fatal("MergeMergeRequest succeeded on 406, want error")
	}
	if !errors.Is(err, provider.ErrInvalidState) {
		t.Fatalf("error = %v, want provider.ErrInvalidState", err)
	}
	if got := provider.HTTPStatus(err); got != http.StatusNotAcceptable {
		t.Errorf("HTTPStatus = %d, want 406", got)
	}
}

func TestListMergeRequestsMapsLabels(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"iid": 1, "title": "renovate", "labels": []string{"renovate"}},
			{"iid": 2, "title": "plain"},
		})
	}))

	mrs, err := c.ListMergeRequests(context.Background(), "team/app", provider.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListMergeRequests: %v", err)
	}
	if len(mrs) != 2 {
		t.Fatalf("len = %d, want 2", len(mrs))
	}
	if !mrs[0].LabelsKnown || !reflect.DeepEqual(mrs[0].Labels, []string{"renovate"}) {
		t.Errorf("mrs[0] labels = %v (known=%v), want [renovate] known", mrs[0].Labels, mrs[0].LabelsKnown)
	}
	if mrs[1].LabelsKnown || mrs[1].Labels != nil {
		t.Errorf("mrs[1] labels = %v (known=%v), want unknown when the field is absent", mrs[1].Labels, mrs[1].LabelsKnown)
	}
}

func newScopedTestClient(t *testing.T, handler http.Handler, scope string) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(config.ProviderConfig{
		Name:           "p",
		Type:           "gitlab",
		BaseURL:        srv.URL,
		Token:          "test-token",
		RequestTimeout: config.Duration(5 * time.Second),
		ProjectScope:   scope,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestListRepositoriesProjectScope(t *testing.T) {
	tests := []struct {
		name  string
		scope string
		want  string
	}{
		{"accessible", "accessible", "false"},
		{"membership", "membership", "true"},
		{"empty defaults to accessible", "", "false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			c := newScopedTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Query().Get("membership")
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode([]map[string]any{})
			}), tt.scope)

			if _, err := c.ListRepositories(context.Background(), provider.RepoListOptions{Limit: 10}); err != nil {
				t.Fatalf("ListRepositories: %v", err)
			}
			if got != tt.want {
				t.Errorf("membership = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestListRepositoriesGroupFirst(t *testing.T) {
	var gotPath, gotInclude, gotSearch string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotInclude = r.URL.Query().Get("include_subgroups")
		gotSearch = r.URL.Query().Get("search")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"id":                  1,
				"path_with_namespace": "devops/platform/app",
				"web_url":             "https://x/devops/platform/app",
				"topics":              []string{"ai-ok"},
			},
		})
	}))

	repos, err := c.ListRepositories(context.Background(), provider.RepoListOptions{Search: "devops/platform", Limit: 10})
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/groups/devops%2Fplatform/projects") {
		t.Errorf("path = %q, want group endpoint with URL-encoded slash", gotPath)
	}
	if gotInclude != "true" {
		t.Errorf("include_subgroups = %q, want true", gotInclude)
	}
	if gotSearch != "" {
		t.Errorf("group search must not send search, got %q", gotSearch)
	}
	if len(repos) != 1 || repos[0].Path != "devops/platform/app" {
		t.Fatalf("repos = %+v, want devops/platform/app", repos)
	}
	if !repos[0].TopicsKnown || !reflect.DeepEqual(repos[0].Topics, []string{"ai-ok"}) {
		t.Errorf("topics = %v (known=%v), want [ai-ok] known", repos[0].Topics, repos[0].TopicsKnown)
	}
}

func TestListRepositoriesFallsBackToProjectsSearch(t *testing.T) {
	var gotPaths []string
	var gotSearch, gotNamespaces string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/groups/") {
			http.NotFound(w, r)
			return
		}
		gotPaths = append(gotPaths, r.URL.Path)
		gotSearch = r.URL.Query().Get("search")
		gotNamespaces = r.URL.Query().Get("search_namespaces")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "path_with_namespace": "devops/platform/app"},
		})
	}))

	repos, err := c.ListRepositories(context.Background(), provider.RepoListOptions{Search: "devops/platform", Limit: 10})
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(gotPaths) != 1 || gotPaths[0] != "/api/v4/projects" {
		t.Fatalf("project paths = %v, want [/api/v4/projects]", gotPaths)
	}
	if gotSearch != "devops/platform" {
		t.Errorf("search = %q, want devops/platform", gotSearch)
	}
	if gotNamespaces != "true" {
		t.Errorf("search_namespaces = %q, want true", gotNamespaces)
	}
	if len(repos) != 1 || repos[0].Path != "devops/platform/app" {
		t.Fatalf("repos = %+v, want devops/platform/app", repos)
	}
}

func TestListRepositoriesNoSearchOmitsNamespaces(t *testing.T) {
	var values url.Values
	var gotPath string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		values = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))

	if _, err := c.ListRepositories(context.Background(), provider.RepoListOptions{Limit: 10}); err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if gotPath != "/api/v4/projects" {
		t.Errorf("path = %q, want /api/v4/projects", gotPath)
	}
	if values.Has("search") {
		t.Errorf("search = %q, want absent", values.Get("search"))
	}
	if values.Has("search_namespaces") {
		t.Errorf("search_namespaces = %q, want absent when no search term is given", values.Get("search_namespaces"))
	}
}

func TestListMergeRequestDiffs(t *testing.T) {
	var gotPath string
	var pages []string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		w.Header().Set("Content-Type", "application/json")
		if page == "1" {
			w.Header().Set("X-Next-Page", "2")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"old_path": "a.go", "new_path": "a.go", "diff": "+a"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"old_path": "b.go", "new_path": "b.go", "diff": "+b", "new_file": true, "too_large": true},
		})
	}))

	files, err := c.ListMergeRequestDiffs(context.Background(), "team/app", 42)
	if err != nil {
		t.Fatalf("ListMergeRequestDiffs: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/merge_requests/42/diffs") {
		t.Errorf("path = %q, want suffix /merge_requests/42/diffs", gotPath)
	}
	if len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Errorf("pages = %v, want [1 2]", pages)
	}
	if len(files) != 2 {
		t.Fatalf("files = %+v, want 2", files)
	}
	if files[0].Diff != "+a" || files[0].NewFile {
		t.Errorf("files[0] = %+v", files[0])
	}
	if files[1].Diff != "+b" || !files[1].NewFile || !files[1].TooLarge {
		t.Errorf("files[1] = %+v", files[1])
	}
}

func TestListMergeRequestDiffsError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	_, err := c.ListMergeRequestDiffs(context.Background(), "team/app", 42)
	if !errors.Is(err, provider.ErrForbidden) {
		t.Fatalf("error = %v, want provider.ErrForbidden", err)
	}
}

func TestListMergeRequestsPaginates(t *testing.T) {
	var pages []string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/merge_requests") {
			http.NotFound(w, r)
			return
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		w.Header().Set("Content-Type", "application/json")
		if page == "1" {
			w.Header().Set("X-Next-Page", "2")
			items := make([]map[string]any, 100)
			for i := range items {
				items[i] = map[string]any{"iid": i + 1, "title": "t", "state": "opened"}
			}
			_ = json.NewEncoder(w).Encode(items)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"iid": 101, "title": "t", "state": "opened"}})
	}))

	mrs, err := c.ListMergeRequests(context.Background(), "team/app", provider.ListOptions{Limit: 101})
	if err != nil {
		t.Fatalf("ListMergeRequests: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("pages = %v, want two pages", pages)
	}
	if len(mrs) != 101 {
		t.Fatalf("len(mrs) = %d, want 101 (pagination must exceed one page)", len(mrs))
	}
}

func TestListMergeRequestNotesPaginates(t *testing.T) {
	var pages []string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/notes") {
			http.NotFound(w, r)
			return
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		w.Header().Set("Content-Type", "application/json")
		if page == "1" {
			w.Header().Set("X-Next-Page", "2")
			items := make([]map[string]any, 100)
			for i := range items {
				items[i] = map[string]any{"id": i + 1, "body": "n"}
			}
			_ = json.NewEncoder(w).Encode(items)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 101, "body": "n"}})
	}))

	notes, err := c.ListMergeRequestNotes(context.Background(), "team/app", 42, provider.ListOptions{Limit: 101})
	if err != nil {
		t.Fatalf("ListMergeRequestNotes: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("pages = %v, want two pages", pages)
	}
	if len(notes) != 101 {
		t.Fatalf("len(notes) = %d, want 101", len(notes))
	}
}

func TestGitAuthHeader(t *testing.T) {
	c := newTestClient(t, http.NotFoundHandler())

	header, err := c.GitAuthHeader(context.Background(), "team/app")
	if err != nil {
		t.Fatalf("GitAuthHeader: %v", err)
	}
	if !strings.HasPrefix(header, "Basic ") {
		t.Fatalf("GitAuthHeader = %q, want the Basic scheme", header)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Basic "))
	if err != nil {
		t.Fatalf("GitAuthHeader payload is not valid base64: %v", err)
	}
	if want := "oauth2:test-token"; string(raw) != want {
		t.Errorf("decoded credentials = %q, want %q (the git transport needs the configured token)", raw, want)
	}
}

func TestMergeBaseHTTP(t *testing.T) {
	const baseSHA = "8be373d96aa74ad3b7a6bfd11f7f27869df4e499"
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repository/merge_base") {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query()["refs[]"]; !reflect.DeepEqual(got, []string{"main", "feat/ai"}) {
			t.Errorf("refs query = %v, want [main feat/ai]", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": baseSHA})
	}))

	got, err := c.MergeBase(context.Background(), "team/app", "main", "feat/ai")
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	if got != baseSHA {
		t.Errorf("MergeBase = %q, want %q", got, baseSHA)
	}
}

func TestMergeBaseErrorHTTP(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))

	_, err := c.MergeBase(context.Background(), "team/app", "main", "gone")
	if !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("error = %v, want provider.ErrNotFound via mapError", err)
	}
	if status := provider.HTTPStatus(err); status != http.StatusNotFound {
		t.Errorf("HTTPStatus = %d, want 404", status)
	}
}

func TestMergeBaseRequiresRefs(t *testing.T) {
	c := newTestClient(t, http.NotFoundHandler())

	if _, err := c.MergeBase(context.Background(), "team/app"); err == nil {
		t.Fatal("MergeBase without refs succeeded, want an error")
	}
}

func TestGitRemoteURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	c, err := New(config.ProviderConfig{
		Name:    "p",
		Type:    "gitlab",
		BaseURL: srv.URL + "/",
		Token:   "test-token",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := c.GitRemoteURL(context.Background(), "team/app")
	if err != nil {
		t.Fatalf("GitRemoteURL: %v", err)
	}
	if want := srv.URL + "/team/app.git"; got != want {
		t.Errorf("GitRemoteURL = %q, want %q", got, want)
	}
	if strings.Contains(got, "test-token") {
		t.Errorf("GitRemoteURL = %q, must be credential-free", got)
	}
}

// TestGitRemoteURLEscapesReservedChars guards the authorization bypass: a
// repository name with URL delimiters must not change the structure of the
// clone URL. The delimiters have to stay escaped inside the path, the URL
// must parse back to the exact repository path, and no credential may appear.
func TestGitRemoteURLEscapesReservedChars(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	c, err := New(config.ProviderConfig{
		Name:    "p",
		Type:    "gitlab",
		BaseURL: srv.URL,
		Token:   "test-token",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const repo = `team/we?ird#1[m];&=$+`
	got, err := c.GitRemoteURL(context.Background(), repo)
	if err != nil {
		t.Fatalf("GitRemoteURL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("resulting URL is unparsable: %v", err)
	}
	if wantHost := strings.TrimPrefix(srv.URL, "http://"); u.Host != wantHost {
		t.Errorf("host = %q, want the base URL host %q", u.Host, wantHost)
	}
	// Round trip: the path decodes back to exactly the requested repository.
	if wantPath := "/" + repo + ".git"; u.Path != wantPath {
		t.Errorf("path = %q, want %q", u.Path, wantPath)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		t.Errorf("query/fragment = %q/%q, want both empty: delimiters must stay inside the path",
			u.RawQuery, u.Fragment)
	}
	if strings.Contains(got, "test-token") {
		t.Errorf("GitRemoteURL = %q, must be credential-free", got)
	}
	if u.User != nil {
		t.Errorf("GitRemoteURL = %q, must carry no user info", got)
	}
}

func TestDefaultBranchHTTP(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/projects/team/app") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
	}))

	got, err := c.DefaultBranch(context.Background(), "team/app")
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if got != "main" {
		t.Errorf("DefaultBranch = %q, want main", got)
	}
}

func TestResolveRefHTTP(t *testing.T) {
	const tipSHA = "1111111111111111111111111111111111111111"
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/repository/branches/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":   "ai/fix",
			"commit": map[string]any{"id": tipSHA},
		})
	}))

	got, err := c.ResolveRef(context.Background(), "team/app", "ai/fix")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if got != tipSHA {
		t.Errorf("ResolveRef = %q, want %q", got, tipSHA)
	}
}

func TestResolveRefNotFoundHTTP(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "404 Branch Not Found", http.StatusNotFound)
	}))

	_, err := c.ResolveRef(context.Background(), "team/app", "gone")
	if !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("ResolveRef error = %v, want provider.ErrNotFound via mapError", err)
	}
}

func TestCanonicalRepository(t *testing.T) {
	c := &Client{}
	valid := map[string]string{
		"team/app":     "team/app",
		"Team/App":     "team/app",
		"team/sub/APP": "team/sub/app",
	}
	for in, want := range valid {
		got, err := c.CanonicalRepository(in)
		if err != nil || got != want {
			t.Errorf("CanonicalRepository(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// A bare numeric name would be read by GitLab as a project id, not a path.
	for _, in := range []string{"", "app", "42"} {
		if _, err := c.CanonicalRepository(in); err == nil {
			t.Errorf("CanonicalRepository(%q) accepted an invalid repository", in)
		}
	}
}
