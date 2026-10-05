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
			{Name: CapMRComment, Filter: CapabilityFilter{Require: []string{"ai-reviewed"}, Exclude: []string{"do-not-touch"}}},
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
			{Name: CapMRRead, Filter: CapabilityFilter{Require: []string{"ai-reviewed"}}},
		}},
	})
	rules := p.Rules()
	rules[0].Repositories[0] = "mutated"
	rules[0].Capabilities[CapMRRead] = CapabilityFilter{Require: []string{"mutated"}}

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
	if len(KnownCapabilities()) != 8 {
		t.Errorf("KnownCapabilities length = %d, want 8", len(KnownCapabilities()))
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

func TestHasFilter(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/secret"}, Effect: "deny"},
		{Repositories: []string{"team/filtered"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapRepoRead, Filter: CapabilityFilter{Require: []string{"ai-ok"}}},
		}},
		{Repositories: []string{"team/paths"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapRepoRead, Filter: CapabilityFilter{Paths: PathFilter{Include: []string{"docs/**"}}}},
		}},
		{Repositories: []string{"team/plain"}, Effect: "allow", Capabilities: grants(CapRepoRead)},
	})

	if !p.HasFilter("team/filtered", CapRepoRead) {
		t.Error("HasFilter(filtered repo, tag filter) = false, want true")
	}
	if !p.HasFilter("team/paths", CapRepoRead) {
		t.Error("HasFilter(filtered repo, path filter) = false, want true")
	}
	if p.HasFilter("team/filtered", CapRepoList) {
		t.Error("HasFilter(filtered repo, missing capability) = true, want false")
	}
	if p.HasFilter("team/plain", CapRepoRead) {
		t.Error("HasFilter(plain grant) = true, want false")
	}
	if p.HasFilter("team/secret", CapRepoRead) {
		t.Error("HasFilter(deny rule) = true, want false")
	}
	if p.HasFilter("other/repo", CapRepoRead) {
		t.Error("HasFilter(no match) = true, want false")
	}
}

func TestEvaluateResource(t *testing.T) {
	mk := func(f CapabilityFilter) *Policy {
		return mustBuild(t, []RuleSpec{{
			Repositories: []string{"team/app"},
			Effect:       "allow",
			Capabilities: []CapabilityGrant{{Name: CapRepoRead, Filter: f}},
		}})
	}
	tests := []struct {
		name        string
		filter      CapabilityFilter
		tags        TagSet
		path        string
		wantAllowed bool
		wantReason  string
	}{
		{"allow match", CapabilityFilter{Paths: PathFilter{Include: []string{"docs/**"}}}, TagSet{}, "docs/a.md", true, "capability granted"},
		{"allow non-match", CapabilityFilter{Paths: PathFilter{Include: []string{"docs/**"}}}, TagSet{}, "src/a.go", false, "path not allowed"},
		{"exclude match", CapabilityFilter{Paths: PathFilter{Exclude: []string{"**/.env"}}}, TagSet{}, "sub/.env", false, "path excluded"},
		{"exclude-only other path allowed", CapabilityFilter{Paths: PathFilter{Exclude: []string{"**/.env"}}}, TagSet{}, "src/a.go", true, "capability granted"},
		{"exclude wins over include", CapabilityFilter{Paths: PathFilter{Include: []string{"src/**"}, Exclude: []string{"src/secret/**"}}}, TagSet{}, "src/secret/x", false, "path excluded"},
		{"empty path fails closed", CapabilityFilter{Paths: PathFilter{Include: []string{"docs/**"}}}, TagSet{}, "", false, "path required"},
		{"no filter allows", CapabilityFilter{}, TagSet{}, "anything", true, "capability granted"},
		{"tags and path both pass", CapabilityFilter{Require: []string{"ai-ok"}, Paths: PathFilter{Include: []string{"docs/**"}}}, TagSet{Known: true, Values: []string{"ai-ok"}}, "docs/a.md", true, "capability granted"},
		{"tags fail", CapabilityFilter{Require: []string{"ai-ok"}, Paths: PathFilter{Include: []string{"docs/**"}}}, TagSet{Known: true, Values: []string{"other"}}, "docs/a.md", false, "tag requirement not met"},
		{"unknown tags fail before path", CapabilityFilter{Require: []string{"ai-ok"}, Paths: PathFilter{Include: []string{"docs/**"}}}, TagSet{}, "docs/a.md", false, "tag information unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := mk(tt.filter).EvaluateResource("team/app", CapRepoRead, tt.tags, tt.path)
			if d.Allowed != tt.wantAllowed || d.Reason != tt.wantReason {
				t.Errorf("EvaluateResource = %+v, want Allowed=%v Reason=%q", d, tt.wantAllowed, tt.wantReason)
			}
		})
	}
}

func TestEvaluateWithTagsRepoCapability(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapRepoRead, Filter: CapabilityFilter{Require: []string{"ai-ok"}, Exclude: []string{"confidential"}}},
		}},
	})
	if d := p.EvaluateWithTags("team/app", CapRepoRead, TagSet{Known: true, Values: []string{"ai-ok"}}); !d.Allowed {
		t.Errorf("matching topics denied: %+v", d)
	}
	if d := p.EvaluateWithTags("team/app", CapRepoRead, TagSet{Known: true, Values: []string{"ai-ok", "confidential"}}); d.Allowed {
		t.Errorf("excluded topic allowed: %+v", d)
	}
	if d := p.EvaluateWithTags("team/app", CapRepoRead, TagSet{}); d.Allowed {
		t.Errorf("unknown topics allowed an active filter: %+v", d)
	}
}

func TestGrantsAnywhereWithFilter(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/**"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapMRRead, Filter: CapabilityFilter{Require: []string{"ai-reviewed"}}},
		}},
	})
	if !p.GrantsAnywhere(CapMRRead) {
		t.Error("GrantsAnywhere(mr:read) = false, want true even with an active filter")
	}
	if p.GrantsAnywhere(CapMRComment) {
		t.Error("GrantsAnywhere(mr:comment) = true, want false")
	}
}

func TestListSearchPrefixes(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"archive/**", "team/*", "foo/bar", "devops/platform/**", "**", "x/*", "archive/**"}, Effect: "allow", Capabilities: grants(CapRepoList)},
		{Repositories: []string{"other/**"}, Effect: "allow", Capabilities: grants(CapMRRead)},
	})
	got := p.ListSearchPrefixes(CapRepoList)
	want := []string{"archive", "devops/platform", "foo/bar", "team", "x"}
	if len(got) != len(want) {
		t.Fatalf("ListSearchPrefixes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListSearchPrefixes = %v, want %v", got, want)
		}
	}
	if prefixes := p.ListSearchPrefixes(CapRepoRead); len(prefixes) != 0 {
		t.Errorf("ListSearchPrefixes(repo:read) = %v, want none", prefixes)
	}
}
