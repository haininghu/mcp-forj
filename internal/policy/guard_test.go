package policy

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

type fakeChecker struct {
	exists bool
	err    error
}

func (f fakeChecker) FileExists(context.Context, string, string, string) (bool, error) {
	return f.exists, f.err
}

func testGuard(t *testing.T, checker FileChecker) (*Guard, *bytes.Buffer) {
	t.Helper()
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: []string{"mr:read", "mr:comment"}},
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: []string{"mr:read"}},
	})
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	g := NewGuard(
		map[string]*Policy{"fake": p},
		map[string]FileChecker{"fake": checker},
		".noai",
		logger,
	)
	return g, &buf
}

func TestGuardAllowsWhenMarkerAbsent(t *testing.T) {
	g, logs := testGuard(t, fakeChecker{exists: false})
	if err := g.Authorize(context.Background(), "fake", "team/app", CapMRRead); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !strings.Contains(logs.String(), "decision=allow") {
		t.Errorf("logs missing allow decision: %s", logs.String())
	}
}

func TestGuardDeniesWhenMarkerPresent(t *testing.T) {
	g, logs := testGuard(t, fakeChecker{exists: true})
	err := g.Authorize(context.Background(), "fake", "team/app", CapMRRead)
	if !errors.Is(err, ErrNoAI) {
		t.Fatalf("error = %v, want ErrNoAI", err)
	}
	if !strings.Contains(logs.String(), "exists=true") {
		t.Errorf("logs missing marker result: %s", logs.String())
	}
}

func TestGuardFailsClosedOnCheckerError(t *testing.T) {
	g, _ := testGuard(t, fakeChecker{err: errors.New("network down")})
	err := g.Authorize(context.Background(), "fake", "team/app", CapMRRead)
	if !errors.Is(err, ErrMarkerCheck) {
		t.Fatalf("error = %v, want ErrMarkerCheck", err)
	}
}

func TestGuardUnknownProvider(t *testing.T) {
	g, _ := testGuard(t, fakeChecker{})
	err := g.Authorize(context.Background(), "missing", "team/app", CapMRRead)
	if !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("error = %v, want ErrUnknownProvider", err)
	}
}

func TestGuardUnknownRepository(t *testing.T) {
	g, _ := testGuard(t, fakeChecker{})
	err := g.Authorize(context.Background(), "fake", "other/repo", CapMRRead)
	if !errors.Is(err, ErrUnknownRepository) {
		t.Fatalf("error = %v, want ErrUnknownRepository", err)
	}
}

func TestGuardDeniedCapability(t *testing.T) {
	g, _ := testGuard(t, fakeChecker{})
	err := g.Authorize(context.Background(), "fake", "team/app", CapRepoRead)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("error = %v, want ErrDenied", err)
	}
}

func TestConfiguredRulesSortedByProvider(t *testing.T) {
	p1 := mustBuild(t, []RuleSpec{{Repositories: []string{"a/*"}, Effect: "allow", Capabilities: []string{"mr:read"}}})
	p2 := mustBuild(t, []RuleSpec{{Repositories: []string{"b/*"}, Effect: "deny"}})
	g := NewGuard(
		map[string]*Policy{"zebra": p1, "alpha": p2},
		map[string]FileChecker{},
		".noai",
		nil,
	)
	rules := g.ConfiguredRules()
	if len(rules) != 2 {
		t.Fatalf("len = %d, want 2", len(rules))
	}
	if rules[0].Provider != "alpha" || rules[1].Provider != "zebra" {
		t.Errorf("providers out of order: %s, %s", rules[0].Provider, rules[1].Provider)
	}
	if len(rules[1].Capabilities) != 1 || rules[1].Capabilities[0] != CapMRRead {
		t.Errorf("capabilities = %v, want [mr:read]", rules[1].Capabilities)
	}
}
