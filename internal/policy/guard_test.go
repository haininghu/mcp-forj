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
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: grants(CapMRRead, CapMRComment, CapRepoRead)},
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: grants(CapMRRead)},
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
	if err := g.Authorize(context.Background(), "fake", "team/app", CapRepoRead); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !strings.Contains(logs.String(), "decision=allow") {
		t.Errorf("logs missing allow decision: %s", logs.String())
	}
}

func TestGuardDeniesWhenMarkerPresent(t *testing.T) {
	g, logs := testGuard(t, fakeChecker{exists: true})
	err := g.Authorize(context.Background(), "fake", "team/app", CapRepoRead)
	if !errors.Is(err, ErrNoAI) {
		t.Fatalf("error = %v, want ErrNoAI", err)
	}
	if !strings.Contains(logs.String(), "exists=true") {
		t.Errorf("logs missing marker result: %s", logs.String())
	}
}

func TestGuardFailsClosedOnCheckerError(t *testing.T) {
	boom := errors.New("network down")
	g, _ := testGuard(t, fakeChecker{err: boom})
	err := g.Authorize(context.Background(), "fake", "team/app", CapRepoRead)
	if !errors.Is(err, ErrMarkerCheck) {
		t.Fatalf("error = %v, want ErrMarkerCheck", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("underlying checker error is not wrapped/reachable: %v", err)
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
	err := g.Authorize(context.Background(), "fake", "team/app", CapRepoList)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("error = %v, want ErrDenied", err)
	}
}

func TestAuthorizeList(t *testing.T) {
	granted := mustBuild(t, []RuleSpec{
		{Repositories: []string{"archive/**"}, Effect: "allow", Capabilities: grants(CapRepoList)},
	})
	notGranted := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/**"}, Effect: "allow", Capabilities: grants(CapMRRead)},
	})
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	g := NewGuard(
		map[string]*Policy{"granted": granted, "not-granted": notGranted},
		map[string]FileChecker{},
		".noai",
		logger,
	)

	if err := g.AuthorizeList("granted"); err != nil {
		t.Fatalf("AuthorizeList(granted) = %v, want nil", err)
	}
	err := g.AuthorizeList("not-granted")
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("AuthorizeList(not-granted) = %v, want ErrDenied", err)
	}
	err = g.AuthorizeList("missing")
	if !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("AuthorizeList(missing) = %v, want ErrUnknownProvider", err)
	}
	if !strings.Contains(logs.String(), "repo:list") {
		t.Errorf("logs do not mention repo:list: %s", logs.String())
	}
}

func TestGuardStaticRepositoriesAndEvaluate(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app", "archive/**"}, Effect: "allow", Capabilities: grants(CapRepoList)},
	})
	g := NewGuard(map[string]*Policy{"fake": p}, map[string]FileChecker{}, ".noai", nil)

	static, err := g.StaticRepositories("fake")
	if err != nil {
		t.Fatalf("StaticRepositories: %v", err)
	}
	if len(static) != 1 || static[0] != "team/app" {
		t.Errorf("StaticRepositories = %v, want [team/app]", static)
	}
	if _, err := g.StaticRepositories("missing"); !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("StaticRepositories(missing) = %v, want ErrUnknownProvider", err)
	}

	decision, err := g.Evaluate("fake", "archive/a", CapRepoList)
	if err != nil || !decision.Allowed || !decision.Matched {
		t.Errorf("Evaluate(archive/a) = (%+v, %v), want allowed+matched", decision, err)
	}
	decision, err = g.Evaluate("fake", "team/other", CapRepoList)
	if err != nil || decision.Allowed || decision.Matched {
		t.Errorf("Evaluate(team/other) = (%+v, %v), want not allowed+not matched", decision, err)
	}
	if _, err := g.Evaluate("missing", "x", CapRepoList); !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("Evaluate(missing) = %v, want ErrUnknownProvider", err)
	}
}

func TestAuthorizeWithTags(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapMRComment, Filter: CapabilityFilter{Require: []string{"ai-reviewed"}, Exclude: []string{"do-not-touch"}}},
		}},
	})
	g := NewGuard(map[string]*Policy{"fake": p}, map[string]FileChecker{"fake": fakeChecker{}}, ".noai", nil)
	ctx := context.Background()

	// Unknown tags fail closed.
	if err := g.AuthorizeWithTags(ctx, "fake", "team/app", CapMRComment, TagSet{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("unknown tags error = %v, want ErrDenied", err)
	}
	// Matching tags allow.
	if err := g.AuthorizeWithTags(ctx, "fake", "team/app", CapMRComment, TagSet{Known: true, Values: []string{"ai-reviewed"}}); err != nil {
		t.Fatalf("matching tags error = %v, want nil", err)
	}
	// Excluded tag denies.
	if err := g.AuthorizeWithTags(ctx, "fake", "team/app", CapMRComment, TagSet{Known: true, Values: []string{"ai-reviewed", "do-not-touch"}}); !errors.Is(err, ErrDenied) {
		t.Fatalf("excluded tag error = %v, want ErrDenied", err)
	}
}

func TestAuthorizeWithTagsMarkerStillApplies(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapRepoRead, Filter: CapabilityFilter{Require: []string{"ai-ok"}}},
		}},
	})
	g := NewGuard(map[string]*Policy{"fake": p}, map[string]FileChecker{"fake": fakeChecker{exists: true}}, ".noai", nil)
	err := g.AuthorizeWithTags(context.Background(), "fake", "team/app", CapRepoRead, TagSet{Known: true, Values: []string{"ai-ok"}})
	if !errors.Is(err, ErrNoAI) {
		t.Fatalf("error = %v, want ErrNoAI even when tags match", err)
	}
}

func TestMarkerProtectsOnlyRepoContents(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: grants(
			CapRepoRead, CapRepoWrite, CapRepoList, CapMRRead, CapMRComment, CapRebase,
		)},
	})
	g := NewGuard(map[string]*Policy{"fake": p}, map[string]FileChecker{"fake": fakeChecker{exists: true}}, ".noai", nil)
	ctx := context.Background()

	// The marker protects repository-content operations.
	for _, c := range []Capability{CapRepoRead, CapRepoWrite} {
		if err := g.Authorize(ctx, "fake", "team/app", c); !errors.Is(err, ErrNoAI) {
			t.Errorf("Authorize(%s) on .noai repo = %v, want ErrNoAI", c, err)
		}
	}
	// It does not affect listing or merge-request operations.
	for _, c := range []Capability{CapRepoList, CapMRRead, CapMRComment, CapRebase} {
		if err := g.Authorize(ctx, "fake", "team/app", c); err != nil {
			t.Errorf("Authorize(%s) on .noai repo = %v, want nil", c, err)
		}
	}
}

func TestMarkerPresentDeniesRepoWrite(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: grants(CapRepoWrite)},
	})
	g := NewGuard(map[string]*Policy{"fake": p}, map[string]FileChecker{"fake": fakeChecker{exists: true}}, ".noai", nil)
	if err := g.Authorize(context.Background(), "fake", "team/app", CapRepoWrite); !errors.Is(err, ErrNoAI) {
		t.Fatalf("Authorize(repo:write) on .noai repo = %v, want ErrNoAI", err)
	}
}

func TestAuthorizeRepoCapability(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapMRComment, Filter: CapabilityFilter{Require: []string{"ai-reviewed"}}},
		}},
	})
	g := NewGuard(map[string]*Policy{"fake": p}, map[string]FileChecker{"fake": fakeChecker{exists: true}}, ".noai", nil)
	ctx := context.Background()

	// Ignores the tag filter and the marker, but requires the capability.
	if err := g.AuthorizeRepoCapability(ctx, "fake", "team/app", CapMRComment); err != nil {
		t.Fatalf("AuthorizeRepoCapability = %v, want nil", err)
	}
	if err := g.AuthorizeRepoCapability(ctx, "fake", "team/app", CapMRRead); !errors.Is(err, ErrDenied) {
		t.Fatalf("AuthorizeRepoCapability(unGranted) = %v, want ErrDenied", err)
	}
	if err := g.AuthorizeRepoCapability(ctx, "fake", "other/repo", CapMRComment); !errors.Is(err, ErrUnknownRepository) {
		t.Fatalf("AuthorizeRepoCapability(unknown repo) = %v, want ErrUnknownRepository", err)
	}
	if err := g.AuthorizeRepoCapability(ctx, "missing", "team/app", CapMRComment); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("AuthorizeRepoCapability(missing provider) = %v, want ErrUnknownProvider", err)
	}
}

func TestConfiguredRulesSortedByProvider(t *testing.T) {
	p1 := mustBuild(t, []RuleSpec{{Repositories: []string{"a/*"}, Effect: "allow", Capabilities: grants(CapMRRead)}})
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
	if len(rules[1].Capabilities) != 1 || rules[1].Capabilities[0].Name != CapMRRead {
		t.Errorf("capabilities = %v, want [mr:read]", rules[1].Capabilities)
	}
}
