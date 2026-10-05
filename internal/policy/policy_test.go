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

// grants builds unfiltered capability grants from names.
func grants(names ...Capability) []CapabilityGrant {
	out := make([]CapabilityGrant, len(names))
	for i, name := range names {
		out[i] = CapabilityGrant{Name: name}
	}
	return out
}

func TestEvaluate(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/secret"}, Effect: "deny"},
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: grants(CapMRRead)},
		{Repositories: []string{"org/**"}, Effect: "allow", Capabilities: grants(CapRepoRead)},
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

func TestEvaluateWithTags(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/secret"}, Effect: "deny"},
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapMRRead},
			{Name: CapMRComment, Filter: TagFilter{Require: []string{"ai-reviewed"}, Exclude: []string{"do-not-touch"}}},
		}},
	})

	tests := []struct {
		name                  string
		repo                  string
		capability            Capability
		tags                  TagSet
		wantAllowed           bool
		wantCapabilityGranted bool
		wantMatched           bool
		wantReason            string
	}{
		{"zero filter unaffected by unknown tags", "team/app", CapMRRead, TagSet{}, true, true, true, "capability granted"},
		{"active filter fails closed on unknown tags", "team/app", CapMRComment, TagSet{}, false, true, true, "tag information unavailable"},
		{"require all present", "team/app", CapMRComment, TagSet{Known: true, Values: []string{"ai-reviewed", "other"}}, true, true, true, "capability granted"},
		{"require missing", "team/app", CapMRComment, TagSet{Known: true, Values: []string{"other"}}, false, true, true, "tag requirement not met"},
		{"exclude present", "team/app", CapMRComment, TagSet{Known: true, Values: []string{"ai-reviewed", "do-not-touch"}}, false, true, true, "excluded tag present"},
		{"capability not granted by allow rule", "team/app", CapMRWrite, TagSet{Known: true}, false, false, true, "capability mr:write not granted"},
		{"deny rule", "team/secret", CapMRRead, TagSet{Known: true, Values: []string{"ai-reviewed"}}, false, false, true, "denied by rule"},
		{"unknown repository", "other/x", CapMRRead, TagSet{Known: true}, false, false, false, "no matching rule"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.EvaluateWithTags(tt.repo, tt.capability, tt.tags)
			if got.Allowed != tt.wantAllowed || got.CapabilityGranted != tt.wantCapabilityGranted ||
				got.Matched != tt.wantMatched || got.Reason != tt.wantReason {
				t.Errorf("EvaluateWithTags = %+v, want {Allowed:%v CapabilityGranted:%v Matched:%v Reason:%q}",
					got, tt.wantAllowed, tt.wantCapabilityGranted, tt.wantMatched, tt.wantReason)
			}
		})
	}
}

func TestRulesReturnsCopy(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapMRRead, Filter: TagFilter{Require: []string{"ai-reviewed"}}},
		}},
	})
	rules := p.Rules()
	rules[0].Repositories[0] = "mutated"
	rules[0].Capabilities[CapMRRead] = TagFilter{Require: []string{"mutated"}}

	again := p.Rules()
	if again[0].Repositories[0] != "team/*" {
		t.Error("Rules did not return a copy of repositories")
	}
	filter := again[0].Capabilities[CapMRRead]
	if len(filter.Require) != 1 || filter.Require[0] != "ai-reviewed" {
		t.Errorf("Rules did not return a copy of capabilities: %+v", filter)
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
		{"unknown capability", []RuleSpec{{Repositories: []string{"a/b"}, Effect: "allow", Capabilities: grants("repo:teleport")}}},
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

func TestIsMRCapability(t *testing.T) {
	for _, c := range []Capability{CapMRRead, CapMRDiff, CapMRComment, CapMRWrite} {
		if !IsMRCapability(c) {
			t.Errorf("IsMRCapability(%s) = false, want true", c)
		}
	}
	for _, c := range []Capability{CapRepoList, CapRepoRead, CapRepoWrite} {
		if IsMRCapability(c) {
			t.Errorf("IsMRCapability(%s) = true, want false", c)
		}
	}
}

func TestClassify(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/secret"}, Effect: "deny"},
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: grants(CapMRRead)},
	})
	if matched, effect := p.Classify("team/secret"); !matched || effect != EffectDeny {
		t.Errorf("Classify(team/secret) = (%v, %q), want (true, deny)", matched, effect)
	}
	if matched, effect := p.Classify("team/app"); !matched || effect != EffectAllow {
		t.Errorf("Classify(team/app) = (%v, %q), want (true, allow)", matched, effect)
	}
	if matched, _ := p.Classify("other/x"); matched {
		t.Error("Classify(other/x) matched, want no match")
	}
}

func TestStaticRepositories(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/secret"}, Effect: "deny"},
		{Repositories: []string{"team/app", "archive/**", "team/secret", "legacy/lit"}, Effect: "allow", Capabilities: grants(CapRepoList)},
	})
	got := p.StaticRepositories()
	want := []string{"team/app", "legacy/lit"}
	if len(got) != len(want) {
		t.Fatalf("StaticRepositories = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("StaticRepositories = %v, want %v", got, want)
		}
	}
}

func TestIsLiteralPattern(t *testing.T) {
	literals := []string{"team/app", "a/b/c", "team/service-a"}
	glob := []string{"team/*", "archive/**", "a?b", "a[bc]", "{a,b}", `a\b`}
	for _, p := range literals {
		if !isLiteralPattern(p) {
			t.Errorf("isLiteralPattern(%q) = false, want true", p)
		}
	}
	for _, p := range glob {
		if isLiteralPattern(p) {
			t.Errorf("isLiteralPattern(%q) = true, want false", p)
		}
	}
}

func TestGrantsAnywhere(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"archive/**"}, Effect: "allow", Capabilities: grants(CapRepoList)},
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

func TestGrantsAnywhereWithFilter(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/**"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapMRRead, Filter: TagFilter{Require: []string{"ai-reviewed"}}},
		}},
	})
	if !p.GrantsAnywhere(CapMRRead) {
		t.Error("GrantsAnywhere(mr:read) = false, want true even with an active filter")
	}
	if p.GrantsAnywhere(CapMRComment) {
		t.Error("GrantsAnywhere(mr:comment) = true, want false")
	}
}
