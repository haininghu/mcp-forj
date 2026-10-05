package policy

import "testing"

func mustBuild(t *testing.T, specs []RuleSpec) *Policy {
	t.Helper()
	p, err := Build(specs)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return p
}

func TestEvaluate(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/secret"}, Effect: "deny"},
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: []string{"mr:read"}},
		{Repositories: []string{"org/**"}, Effect: "allow", Capabilities: []string{"repo:read"}},
	})

	tests := []struct {
		name        string
		repo        string
		capability  Capability
		wantAllowed bool
		wantMatched bool
	}{
		{"specific deny wins over broad allow", "team/secret", CapMRRead, false, true},
		{"broad allow grants listed capability", "team/app", CapMRRead, true, true},
		{"broad allow withholds unlisted capability", "team/app", CapMRComment, false, true},
		{"single star does not cross segments", "team/sub/app", CapMRRead, false, false},
		{"double star crosses segments", "org/sub/app", CapRepoRead, true, true},
		{"unknown repository denied", "other/repo", CapMRRead, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.Evaluate(tt.repo, tt.capability)
			if got.Allowed != tt.wantAllowed || got.Matched != tt.wantMatched {
				t.Errorf("Evaluate(%q, %q) = {Allowed:%v Matched:%v Reason:%q}, want {Allowed:%v Matched:%v}",
					tt.repo, tt.capability, got.Allowed, got.Matched, got.Reason, tt.wantAllowed, tt.wantMatched)
			}
		})
	}
}

func TestEvaluateDefaultDeny(t *testing.T) {
	p := mustBuild(t, nil)
	got := p.Evaluate("team/app", CapMRRead)
	if got.Allowed || got.Matched {
		t.Fatalf("empty policy allowed access: %+v", got)
	}
	if got.Reason != "no matching rule" {
		t.Errorf("reason = %q, want no matching rule", got.Reason)
	}
}

func TestRulesReturnsCopy(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: []string{"mr:read"}},
	})
	rules := p.Rules()
	rules[0].Repositories[0] = "mutated"
	rules[0].Capabilities[CapMRRead] = false

	again := p.Rules()
	if again[0].Repositories[0] != "team/*" {
		t.Error("Rules did not return a copy of repositories")
	}
	if !again[0].Capabilities[CapMRRead] {
		t.Error("Rules did not return a copy of capabilities")
	}
}

func TestBuildValidation(t *testing.T) {
	tests := []struct {
		name  string
		specs []RuleSpec
	}{
		{"unknown effect", []RuleSpec{{Repositories: []string{"a/b"}, Effect: "maybe"}}},
		{"empty repositories", []RuleSpec{{Effect: "allow"}}},
		{"empty pattern", []RuleSpec{{Repositories: []string{""}, Effect: "allow"}}},
		{"unknown capability", []RuleSpec{{Repositories: []string{"a/b"}, Effect: "allow", Capabilities: []string{"repo:teleport"}}}},
		{"invalid pattern", []RuleSpec{{Repositories: []string{"a/["}, Effect: "allow"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Build(tt.specs); err == nil {
				t.Fatal("Build succeeded, want error")
			}
		})
	}
}

func TestKnownCapabilities(t *testing.T) {
	if !IsKnownCapability("mr:read") {
		t.Error("mr:read should be known")
	}
	if !IsKnownCapability("repo:list") {
		t.Error("repo:list should be known")
	}
	if IsKnownCapability("repo:teleport") {
		t.Error("repo:teleport should not be known")
	}
	if len(KnownCapabilities()) != 7 {
		t.Errorf("KnownCapabilities length = %d, want 7", len(KnownCapabilities()))
	}
}

func TestGrantsAnywhere(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"archive/**"}, Effect: "allow", Capabilities: []string{"repo:list"}},
		{Repositories: []string{"team/**"}, Effect: "deny"},
	})
	if !p.GrantsAnywhere(CapRepoList) {
		t.Error("GrantsAnywhere(repo:list) = false, want true")
	}
	if p.GrantsAnywhere(CapRepoRead) {
		t.Error("GrantsAnywhere(repo:read) = true, want false")
	}
	if p.GrantsAnywhere(CapMRRead) {
		t.Error("GrantsAnywhere(mr:read) = true, want false")
	}
}
