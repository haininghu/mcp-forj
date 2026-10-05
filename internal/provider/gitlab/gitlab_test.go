package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
		Token:          config.Secret("test-token"),
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
		if got := r.URL.Query().Get("membership"); got != "true" {
			t.Errorf("membership = %q, want true", got)
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
			if got := mapError(err); !errors.Is(got, tt.want) {
				t.Errorf("mapError(%d) = %v, want %v", tt.status, got, tt.want)
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
	})
}

func TestGetMergeRequestNotFoundHTTP(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	_, err := c.GetMergeRequest(context.Background(), "team/app", 1)
	if err != provider.ErrNotFound {
		t.Fatalf("error = %v, want provider.ErrNotFound", err)
	}
}

func TestFileExistsHTTP(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	if err != provider.ErrNotFound {
		t.Fatalf("error = %v, want provider.ErrNotFound", err)
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
	if strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("error leaks raw HTTP detail: %v", err)
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
