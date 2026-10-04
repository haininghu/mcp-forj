package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if got != want {
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
	t.Setenv("MCP_FORJ_TEST_MISSING", "")
	_, err := New(config.ProviderConfig{
		Name:     "p",
		Type:     "gitlab",
		BaseURL:  "https://gitlab.example.com",
		TokenEnv: "MCP_FORJ_TEST_MISSING",
	})
	if err == nil {
		t.Fatal("New succeeded with an unset token, want error")
	}
	if !strings.Contains(err.Error(), "MCP_FORJ_TEST_MISSING") {
		t.Errorf("error = %q, want it to name the env var", err)
	}
}

func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("MCP_FORJ_TEST_TOKEN", "test-token")
	c, err := New(config.ProviderConfig{
		Name:           "p",
		Type:           "gitlab",
		BaseURL:        srv.URL,
		TokenEnv:       "MCP_FORJ_TEST_TOKEN",
		RequestTimeout: config.Duration(5 * time.Second),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestGetMergeRequestHTTP(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/merge_requests/42") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"iid":           42,
			"title":         "Add feature",
			"state":         "opened",
			"source_branch": "feat",
			"target_branch": "main",
			"web_url":       "https://example.com/mr/42",
			"author":        map[string]any{"username": "alice"},
		})
	}))

	mr, err := c.GetMergeRequest(context.Background(), "team/app", 42)
	if err != nil {
		t.Fatalf("GetMergeRequest: %v", err)
	}
	if mr.Number != 42 || mr.Author != "alice" || mr.Title != "Add feature" {
		t.Errorf("unexpected merge request: %+v", mr)
	}
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
