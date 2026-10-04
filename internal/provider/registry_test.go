package provider

import (
	"context"
	"testing"
)

type stubProvider struct{ name, typ string }

func (s stubProvider) Name() string { return s.name }
func (s stubProvider) Type() string { return s.typ }
func (s stubProvider) ListMergeRequests(context.Context, string, ListOptions) ([]MergeRequest, error) {
	return nil, nil
}
func (s stubProvider) GetMergeRequest(context.Context, string, int64) (*MergeRequest, error) {
	return nil, ErrNotFound
}
func (s stubProvider) ListMergeRequestNotes(context.Context, string, int64) ([]Note, error) {
	return nil, nil
}
func (s stubProvider) AddMergeRequestNote(context.Context, string, int64, string) (*Note, error) {
	return nil, nil
}
func (s stubProvider) ReadFile(context.Context, string, string, string) ([]byte, error) {
	return nil, ErrNotFound
}
func (s stubProvider) FileExists(context.Context, string, string, string) (bool, error) {
	return false, nil
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	r.Register(stubProvider{name: "zeta", typ: "gitlab"})
	r.Register(stubProvider{name: "alpha", typ: "gitlab"})

	if _, ok := r.Get("alpha"); !ok {
		t.Error("Get(alpha) not found")
	}
	if _, ok := r.Get("missing"); ok {
		t.Error("Get(missing) found")
	}
	names := r.Names()
	if len(names) != 2 || names[0] != "alpha" || names[1] != "zeta" {
		t.Errorf("Names = %v, want [alpha zeta]", names)
	}
	all := r.All()
	all["mutated"] = stubProvider{name: "mutated"}
	if _, ok := r.Get("mutated"); ok {
		t.Error("All did not return a copy")
	}
}
