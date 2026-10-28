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
		name        string
		repo        string
		capability  Capability
		tags        TagSet
		wantAllowed bool
		wantMatched bool
		wantReason  string
	}{
		{"zero filter unaffected by unknown tags", "team/app", CapMRRead, TagSet{}, true, true, "capability granted"},
		{"active filter fails closed on unknown tags", "team/app", CapMRComment, TagSet{}, false, true, "tag information unavailable"},
		{"require all present", "team/app", CapMRComment, TagSet{Known: true, Values: []string{"ai-reviewed", "other"}}, true, true, "capability granted"},
		{"require missing falls through", "team/app", CapMRComment, TagSet{Known: true, Values: []string{"other"}}, false, true, "tag requirement not met"},
		{"exclude present falls through", "team/app", CapMRComment, TagSet{Known: true, Values: []string{"ai-reviewed", "do-not-touch"}}, false, true, "excluded tag present"},
		{"capability not granted by allow rule", "team/app", CapMRWrite, TagSet{Known: true}, false, true, "capability mr:write not granted"},
		{"deny rule", "team/secret", CapMRRead, TagSet{Known: true, Values: []string{"ai-reviewed"}}, false, true, "denied by rule"},
		{"unknown repository", "other/x", CapMRRead, TagSet{Known: true}, false, false, "no matching rule"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.EvaluateWithTags(tt.repo, tt.capability, tt.tags)
			if got.Allowed != tt.wantAllowed || got.Matched != tt.wantMatched || got.Reason != tt.wantReason {
				t.Errorf("EvaluateWithTags = %+v, want {Allowed:%v Matched:%v Reason:%q}",
					got, tt.wantAllowed, tt.wantMatched, tt.wantReason)
			}
		})
	}
}

func TestEvaluateNoAIExempt(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapRepoRead, Filter: CapabilityFilter{NoAIExempt: true}},
			{Name: CapRepoList},
			{Name: CapMRRead, Filter: CapabilityFilter{NoAIExempt: true, Require: []string{"ai-ok"}}},
		}},
	})

	if got := p.Evaluate("team/app", CapRepoRead); !got.Allowed || !got.NoAIExempt {
		t.Errorf("repo:read = %+v, want allowed + NoAIExempt", got)
	}
	if got := p.Evaluate("team/app", CapRepoList); !got.Allowed || got.NoAIExempt {
		t.Errorf("repo:list = %+v, want allowed without exemption", got)
	}
	// A tag miss makes the rule fall through; no rule then applies, so the
	// decision denies and carries no exemption.
	got := p.EvaluateWithTags("team/app", CapMRRead, TagSet{Known: true, Values: []string{"other"}})
	if got.Allowed || got.NoAIExempt {
		t.Errorf("mr:read tag fall-through = %+v, want denied without exemption", got)
	}
	// Deny/no-match decisions never carry the exemption.
	if got := p.Evaluate("team/secret", CapRepoRead); got.NoAIExempt {
		t.Errorf("unknown repository decision = %+v, want NoAIExempt false", got)
	}
}

func TestNoAIExemptCloneAndIsZero(t *testing.T) {
	if !(CapabilityFilter{NoAIExempt: true}).IsZero() {
		t.Error("NoAIExempt must not affect IsZero")
	}
	f := CapabilityFilter{NoAIExempt: true, Require: []string{"ai-ok"}}
	c := cloneFilter(f)
	if !c.NoAIExempt || len(c.Require) != 1 {
		t.Errorf("cloneFilter = %+v, want NoAIExempt preserved", c)
	}
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapRepoRead, Filter: f},
		}},
	})
	rules := p.Rules()
	if !rules[0].Capabilities[CapRepoRead].NoAIExempt {
		t.Error("Rules did not preserve NoAIExempt")
	}
	sorted := rules[0].SortedCapabilities()
	if len(sorted) != 1 || !sorted[0].Filter.NoAIExempt {
		t.Errorf("SortedCapabilities = %+v, want NoAIExempt preserved", sorted)
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
		{"invalid branch include pattern", []RuleSpec{{Repositories: []string{"a/b"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapRepoWrite, Filter: CapabilityFilter{Branches: PathFilter{Include: []string{"ai/["}}}},
		}}}},
		{"empty branch exclude pattern", []RuleSpec{{Repositories: []string{"a/b"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapRepoWrite, Filter: CapabilityFilter{Branches: PathFilter{Exclude: []string{""}}}},
		}}}},
		{"noai exemption on deny rule", []RuleSpec{{Repositories: []string{"a/b"}, Effect: "deny", Capabilities: []CapabilityGrant{
			{Name: CapRepoRead, Filter: CapabilityFilter{NoAIExempt: true}},
		}}}},
		{"labels on another capability", []RuleSpec{{Repositories: []string{"a/b"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapMRComment, Filter: CapabilityFilter{Labels: LabelFilter{Include: []string{"ai-reviewed"}}}},
		}}}},
		{"empty label in include", []RuleSpec{{Repositories: []string{"a/b"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapMRLabel, Filter: CapabilityFilter{Labels: LabelFilter{Include: []string{""}}}},
		}}}},
		{"label in include and exclude", []RuleSpec{{Repositories: []string{"a/b"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapMRLabel, Filter: CapabilityFilter{Labels: LabelFilter{Include: []string{"x"}, Exclude: []string{"x"}}}},
		}}}},
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
	if !IsKnownCapability("policy:read") {
		t.Error("policy:read should be known")
	}
	if !IsKnownCapability("mr:merge") {
		t.Error("mr:merge should be known")
	}
	if IsKnownCapability("repo:teleport") {
		t.Error("repo:teleport should not be known")
	}
	if !IsKnownCapability("commit:read") {
		t.Error("commit:read should be known")
	}
	if !IsKnownCapability("mr:label") {
		t.Error("mr:label should be known")
	}
	if len(KnownCapabilities()) != 12 {
		t.Errorf("KnownCapabilities length = %d, want 12", len(KnownCapabilities()))
	}
}

func TestListable(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/secret"}, Effect: "deny"},
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: grants(CapMRRead)},
	})
	if p.Listable("team/secret") {
		t.Error("listable(team/secret) = true, want false (scope-only deny hides)")
	}
	if !p.Listable("team/app") {
		t.Error("listable(team/app) = false, want true (allow exposes)")
	}
	if p.Listable("other/x") {
		t.Error("listable(other/x) = true, want false (no match)")
	}
	// A deny rule that does not list repo:list does not hide the repository; a
	// deny rule that does list repo:list does.
	q := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "deny", Capabilities: grants(CapMRRead)},
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: grants(CapRepoList)},
	})
	if !q.Listable("team/app") {
		t.Error("listable(team/app) with a non-repo:list deny = false, want true")
	}
	r := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "deny", Capabilities: grants(CapRepoList)},
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: grants(CapRepoList)},
	})
	if r.Listable("team/app") {
		t.Error("listable(team/app) with a repo:list deny = true, want false")
	}
	// First-match-wins: an allow exposes the listing before a later deny that
	// lists repo:list (the allow does not need repo:list itself).
	s := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: grants(CapMRRead)},
		{Repositories: []string{"team/app"}, Effect: "deny", Capabilities: grants(CapRepoList)},
	})
	if !s.Listable("team/app") {
		t.Error("allow before a repo:list deny: listable = false, want true (first match wins)")
	}
	// A repo named only by a deny rule that lists other capabilities has no
	// exposing allow, so it is not listed.
	d := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "deny", Capabilities: grants(CapMRRead)},
	})
	if d.Listable("team/app") {
		t.Error("deny-only-other-capability: listable = true, want false (no allow)")
	}
}

func TestHasTagConstraint(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/paths"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapRepoRead, Filter: CapabilityFilter{Paths: PathFilter{Include: []string{"docs/**"}}}},
		}},
		{Repositories: []string{"team/paths"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapRepoRead, Filter: CapabilityFilter{Require: []string{"ai-ok"}}},
		}},
		{Repositories: []string{"team/plain"}, Effect: "allow", Capabilities: grants(CapRepoRead)},
		{Repositories: []string{"team/secret"}, Effect: "deny"},
		{Repositories: []string{"team/both"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapRepoRead, Filter: CapabilityFilter{Require: []string{"ai-ok"}, Paths: PathFilter{Include: []string{"docs/**"}}}},
		}},
		{Repositories: []string{"team/both"}, Effect: "allow", Capabilities: grants(CapRepoRead)},
	})
	// A rule with both a tag and a path filter needs tags even when the path
	// matches: the tag dimension is evaluated first.
	if !p.HasTagConstraint("team/both", CapRepoRead, "docs/a.md") {
		t.Error("tag+path rule: HasTagConstraint = false, want true (tags are evaluated first)")
	}
	if !p.HasTagConstraint("team/both", CapRepoRead, "src/a.go") {
		t.Error("tag+path rule (path miss): HasTagConstraint = false, want true")
	}
	// A matching path rule without tags decides without tags, even though a later
	// rule has a tag constraint.
	if p.HasTagConstraint("team/paths", CapRepoRead, "docs/a.md") {
		t.Error("matching path rule: HasTagConstraint = true, want false")
	}
	// A path miss falls through to the tag-constrained rule.
	if !p.HasTagConstraint("team/paths", CapRepoRead, "src/a.go") {
		t.Error("path miss: HasTagConstraint = false, want true")
	}
	// An unfiltered allow decides without tags.
	if p.HasTagConstraint("team/plain", CapRepoRead, "") {
		t.Error("plain grant: HasTagConstraint = true, want false")
	}
	// A scope-only deny decides without tags.
	if p.HasTagConstraint("team/secret", CapRepoRead, "") {
		t.Error("scope-only deny: HasTagConstraint = true, want false")
	}
}

func TestHasFilterStopsAtUnfilteredAllow(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: grants(CapRepoList)},
		{Repositories: []string{"team/app"}, Effect: "deny", Capabilities: []CapabilityGrant{
			{Name: CapRepoList, Filter: CapabilityFilter{Require: []string{"x"}}},
		}},
	})
	if p.HasFilter("team/app", CapRepoList) {
		t.Error("unfiltered allow before a tag deny: HasFilter = true, want false")
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

func TestCommitReadCapability(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapCommitRead, Filter: CapabilityFilter{Require: []string{"ai-ok"}, NoAIExempt: true}},
		}},
	})
	// The tag dimension for commit:read matches project topics.
	if d := p.EvaluateWithTags("team/app", CapCommitRead, TagSet{Known: true, Values: []string{"ai-ok"}}); !d.Allowed || !d.NoAIExempt {
		t.Errorf("commit:read matching topic = %+v, want allowed + NoAIExempt", d)
	}
	if d := p.EvaluateWithTags("team/app", CapCommitRead, TagSet{Known: true, Values: []string{"other"}}); d.Allowed {
		t.Errorf("commit:read missing topic = %+v, want denied", d)
	}
	if d := p.EvaluateWithTags("team/app", CapCommitRead, TagSet{}); d.Allowed || d.Reason != "tag information unavailable" {
		t.Errorf("commit:read unknown topics = %+v, want fail-closed", d)
	}
	// The implication is one-directional: commit:read does not grant repo:read.
	if d := p.Evaluate("team/app", CapRepoRead); d.Allowed {
		t.Errorf("repo:read unexpectedly granted by a commit:read rule: %+v", d)
	}
}

// TestRepoReadImpliesCommitRead pins the implication semantics: a repo:read
// entry authorizes commit:read with the same filter, an explicit commit:read
// entry wins, and the implication is symmetric for deny.
func TestRepoReadImpliesCommitRead(t *testing.T) {
	t.Run("allow repo:read grants commit:read", func(t *testing.T) {
		p := mustBuild(t, []RuleSpec{
			{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: grants(CapRepoRead)},
		})
		if d := p.Evaluate("team/app", CapCommitRead); !d.Allowed {
			t.Errorf("commit:read via repo:read = %+v, want allowed", d)
		}
		if granted, matched := p.EvaluatePreCheck("team/app", CapCommitRead); !granted || !matched {
			t.Errorf("EvaluatePreCheck = (%v, %v), want (true, true)", granted, matched)
		}
		// The implication is one-directional: commit:read does not grant repo:read.
		if d := p.Evaluate("team/app", CapRepoRead); !d.Allowed {
			t.Errorf("repo:read = %+v, want allowed", d)
		}
	})

	t.Run("explicit commit:read deny before a broad repo:read allow wins", func(t *testing.T) {
		p := mustBuild(t, []RuleSpec{
			{Repositories: []string{"team/*"}, Effect: "deny", Capabilities: grants(CapCommitRead)},
			{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: grants(CapRepoRead)},
		})
		if d := p.Evaluate("team/app", CapCommitRead); d.Allowed || d.Reason != "denied by rule" {
			t.Errorf("commit:read = %+v, want denied by the earlier deny rule", d)
		}
		if granted, _ := p.EvaluatePreCheck("team/app", CapCommitRead); granted {
			t.Error("EvaluatePreCheck granted despite an unconditional commit:read deny")
		}
		// repo:read itself is still granted by the broad allow.
		if d := p.Evaluate("team/app", CapRepoRead); !d.Allowed {
			t.Errorf("repo:read = %+v, want allowed", d)
		}
	})

	t.Run("deny repo:read denies commit:read symmetrically", func(t *testing.T) {
		p := mustBuild(t, []RuleSpec{
			{Repositories: []string{"team/*"}, Effect: "deny", Capabilities: grants(CapRepoRead)},
			{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: grants(CapRepoRead, CapCommitRead)},
		})
		if d := p.Evaluate("team/app", CapCommitRead); d.Allowed || d.Reason != "denied by rule" {
			t.Errorf("commit:read = %+v, want denied via the implied repo:read deny", d)
		}
	})

	t.Run("explicit commit:read entry wins over an implied repo:read entry", func(t *testing.T) {
		p := mustBuild(t, []RuleSpec{{
			Repositories: []string{"team/app"},
			Effect:       "allow",
			Capabilities: []CapabilityGrant{
				{Name: CapRepoRead, Filter: CapabilityFilter{Require: []string{"repo-topic"}}},
				{Name: CapCommitRead, Filter: CapabilityFilter{Require: []string{"commit-topic"}}},
			},
		}})
		if d := p.EvaluateWithTags("team/app", CapCommitRead, TagSet{Known: true, Values: []string{"commit-topic"}}); !d.Allowed {
			t.Errorf("explicit commit:read filter = %+v, want allowed", d)
		}
		if d := p.EvaluateWithTags("team/app", CapCommitRead, TagSet{Known: true, Values: []string{"repo-topic"}}); d.Allowed {
			t.Errorf("explicit commit:read filter ignored: %+v", d)
		}
	})

	t.Run("repo:read paths filter fails commit:read closed", func(t *testing.T) {
		p := mustBuild(t, []RuleSpec{
			{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: []CapabilityGrant{
				{Name: CapRepoRead, Filter: CapabilityFilter{Paths: PathFilter{Include: []string{"docs/**"}}}},
			}},
		})
		if d := p.Evaluate("team/app", CapCommitRead); d.Allowed || d.Reason != "path required" {
			t.Errorf("commit:read = %+v, want fail-closed (path required)", d)
		}
	})

	t.Run("repo:read topic filter applies to commit:read", func(t *testing.T) {
		p := mustBuild(t, []RuleSpec{
			{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: []CapabilityGrant{
				{Name: CapRepoRead, Filter: CapabilityFilter{Require: []string{"ai-ok"}}},
			}},
		})
		if !p.HasTagConstraint("team/app", CapCommitRead, "") {
			t.Error("HasTagConstraint(commit:read) = false, want true via the repo:read tag filter")
		}
		if d := p.EvaluateWithTags("team/app", CapCommitRead, TagSet{Known: true, Values: []string{"ai-ok"}}); !d.Allowed {
			t.Errorf("commit:read matching topic = %+v, want allowed", d)
		}
		if d := p.EvaluateWithTags("team/app", CapCommitRead, TagSet{Known: true, Values: []string{"other"}}); d.Allowed {
			t.Errorf("commit:read missing topic = %+v, want denied", d)
		}
		if d := p.EvaluateWithTags("team/app", CapCommitRead, TagSet{}); d.Allowed || d.Reason != "tag information unavailable" {
			t.Errorf("commit:read unknown topics = %+v, want fail-closed", d)
		}
	})

	t.Run("repo:read noai exemption carries to commit:read", func(t *testing.T) {
		p := mustBuild(t, []RuleSpec{
			{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: []CapabilityGrant{
				{Name: CapRepoRead, Filter: CapabilityFilter{NoAIExempt: true}},
			}},
		})
		if d := p.Evaluate("team/app", CapCommitRead); !d.Allowed || !d.NoAIExempt {
			t.Errorf("commit:read = %+v, want allowed + NoAIExempt", d)
		}
		if !p.NoAIExemptAll("team/app", CapCommitRead) {
			t.Error("NoAIExemptAll(commit:read) = false, want true")
		}
	})

	t.Run("discovery seeds include repo:read grants", func(t *testing.T) {
		p := mustBuild(t, []RuleSpec{
			{Repositories: []string{"team/app", "team/**"}, Effect: "allow", Capabilities: grants(CapRepoRead)},
		})
		if got := p.StaticRepositoriesFor(CapCommitRead); len(got) != 1 || got[0] != "team/app" {
			t.Errorf("StaticRepositoriesFor(commit:read) = %v, want [team/app]", got)
		}
		if got := p.ListSearchPrefixes(CapCommitRead); len(got) != 2 || got[0] != "team" || got[1] != "team/app" {
			t.Errorf("ListSearchPrefixes(commit:read) = %v, want [team team/app]", got)
		}
	})
}

func TestStaticRepositoriesFor(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/secret"}, Effect: "deny"},
		{Repositories: []string{"team/app", "archive/**", "team/secret", "legacy/lit"}, Effect: "allow", Capabilities: grants(CapCommitRead)},
		{Repositories: []string{"team/other"}, Effect: "allow", Capabilities: grants(CapMRRead)},
	})
	got := p.StaticRepositoriesFor(CapCommitRead)
	want := []string{"team/app", "legacy/lit"}
	if len(got) != len(want) {
		t.Fatalf("StaticRepositoriesFor = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("StaticRepositoriesFor = %v, want %v", got, want)
		}
	}
	// Only allow rules granting the capability contribute.
	if got := p.StaticRepositoriesFor(CapRepoRead); len(got) != 0 {
		t.Errorf("StaticRepositoriesFor(repo:read) = %v, want none", got)
	}
	// A listing deny hides a literal repository from the seed set.
	q := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "deny", Capabilities: grants(CapRepoList)},
		{Repositories: []string{"team/app", "team/**"}, Effect: "allow", Capabilities: grants(CapCommitRead)},
	})
	if got := q.StaticRepositoriesFor(CapCommitRead); len(got) != 0 {
		t.Errorf("StaticRepositoriesFor with a listing deny = %v, want none", got)
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

// branchGrant builds a policy with a single team/app allow rule granting
// repo:write with the given filter.
func branchGrant(t *testing.T, f CapabilityFilter) *Policy {
	t.Helper()
	return mustBuild(t, []RuleSpec{{
		Repositories: []string{"team/app"},
		Effect:       "allow",
		Capabilities: []CapabilityGrant{{Name: CapRepoWrite, Filter: f}},
	}})
}

func TestEvaluateResourceBranch(t *testing.T) {
	withBranches := CapabilityFilter{Branches: PathFilter{Include: []string{"ai/**"}}}
	tests := []struct {
		name        string
		filter      CapabilityFilter
		tags        TagSet
		path        string
		branch      string
		wantAllowed bool
		wantReason  string
	}{
		{"branch filter missing denies a named branch (fail-closed)", CapabilityFilter{}, TagSet{}, "", "ai/fix", false, "branch filter required"},
		{"no branch and no filter imposes no branch constraint", CapabilityFilter{}, TagSet{}, "", "", true, "capability granted"},
		{"include match", withBranches, TagSet{}, "", "ai/fix", true, "capability granted"},
		{"include non-match", withBranches, TagSet{}, "", "feature/x", false, "branch not allowed"},
		{"empty branch with active filter fails closed", withBranches, TagSet{}, "", "", false, "branch required"},
		{"exclude match", CapabilityFilter{Branches: PathFilter{Exclude: []string{"ai/wip/**"}}}, TagSet{}, "", "ai/wip/tmp", false, "branch excluded"},
		{"exclude-only other branch allowed", CapabilityFilter{Branches: PathFilter{Exclude: []string{"ai/wip/**"}}}, TagSet{}, "", "ai/fix", true, "capability granted"},
		{"exclude wins over include", CapabilityFilter{Branches: PathFilter{Include: []string{"**"}, Exclude: []string{"ai/wip/**"}}}, TagSet{}, "", "ai/wip/tmp", false, "branch excluded"},
		{"path filter runs before branch", CapabilityFilter{Paths: PathFilter{Include: []string{"src/**"}}, Branches: withBranches.Branches}, TagSet{}, "", "ai/fix", false, "path required"},
		{"unknown tags run before branch", CapabilityFilter{Require: []string{"ai-ok"}, Branches: withBranches.Branches}, TagSet{}, "", "ai/fix", false, "tag information unavailable"},
		{"tags paths and branch all pass", CapabilityFilter{Require: []string{"ai-ok"}, Paths: PathFilter{Include: []string{"src/**"}}, Branches: withBranches.Branches},
			TagSet{Known: true, Values: []string{"ai-ok"}}, "src/a.go", "ai/fix", true, "capability granted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := branchGrant(t, tt.filter).EvaluateResourceBranch("team/app", CapRepoWrite, tt.tags, tt.path, tt.branch)
			if d.Allowed != tt.wantAllowed || d.Reason != tt.wantReason {
				t.Errorf("EvaluateResourceBranch = %+v, want Allowed=%v Reason=%q", d, tt.wantAllowed, tt.wantReason)
			}
		})
	}
}

// TestEvaluateResourceIgnoresBranchFilter pins the branch-less evaluation
// context: Evaluate/Authorize-style calls carry no branch, so a branch filter
// imposes no constraint there and the decision matches the pre-branch rules.
func TestEvaluateResourceIgnoresBranchFilter(t *testing.T) {
	p := branchGrant(t, CapabilityFilter{Branches: PathFilter{Include: []string{"ai/**"}}})
	for _, d := range []Decision{
		p.Evaluate("team/app", CapRepoWrite),
		p.EvaluateResource("team/app", CapRepoWrite, TagSet{}, ""),
	} {
		if !d.Allowed || d.Reason != "capability granted" {
			t.Errorf("branch-less evaluation = %+v, want allowed (no branch context)", d)
		}
	}
	if !p.HasFilter("team/app", CapRepoWrite) {
		t.Error("HasFilter(branch-filtered grant) = false, want true")
	}
}

func TestCapabilityFilterBranchesIsZeroAndClone(t *testing.T) {
	f := CapabilityFilter{Branches: PathFilter{Include: []string{"ai/**"}}}
	if f.IsZero() {
		t.Error("branch filter must make the capability filter non-zero")
	}
	c := cloneFilter(f)
	if len(c.Branches.Include) != 1 || c.Branches.Include[0] != "ai/**" {
		t.Errorf("cloneFilter dropped branches: %+v", c)
	}
	c.Branches.Include[0] = "mutated"
	if f.Branches.Include[0] != "ai/**" {
		t.Error("cloneFilter did not copy the branches slice")
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

func TestEvaluateWithLabels(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapMRLabel, Filter: CapabilityFilter{
				Require: []string{"ai-reviewed"},
				Labels:  LabelFilter{Include: []string{"ai-reviewed", "ai-done"}, Exclude: []string{"do-not-touch"}},
			}},
		}},
	})
	tests := []struct {
		name    string
		tags    TagSet
		labels  []string
		allowed bool
		reason  string
	}{
		{"allowed labels", TagSet{Known: true, Values: []string{"ai-reviewed"}}, []string{"ai-done"}, true, "capability granted"},
		{"label not in include falls through", TagSet{Known: true, Values: []string{"ai-reviewed"}}, []string{"other"}, false, "label not allowed"},
		{"excluded label wins", TagSet{Known: true, Values: []string{"ai-reviewed"}}, []string{"do-not-touch"}, false, "label excluded"},
		{"active filter with no labels fails closed", TagSet{Known: true, Values: []string{"ai-reviewed"}}, nil, false, "labels required"},
		{"unknown tags fail closed", TagSet{}, []string{"ai-done"}, false, "tag information unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := p.EvaluateWithLabels("team/app", CapMRLabel, tt.tags, tt.labels)
			if d.Allowed != tt.allowed || d.Reason != tt.reason {
				t.Errorf("EvaluateWithLabels = %+v, want Allowed=%v Reason=%q", d, tt.allowed, tt.reason)
			}
		})
	}
}

// TestEvaluateWithLabelsFallThrough checks the firewall semantics: an evaluated
// label miss falls through to a later applicable allow rule.
func TestEvaluateWithLabelsFallThrough(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapMRLabel, Filter: CapabilityFilter{Labels: LabelFilter{Include: []string{"restricted"}}}},
		}},
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapMRLabel},
		}},
	})
	if d := p.EvaluateWithLabels("team/app", CapMRLabel, TagSet{}, []string{"other"}); !d.Allowed {
		t.Errorf("label fall-through = %+v, want the broader grant to allow", d)
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

// TestFirewallDenyWithCapabilities pins the symmetric model: a deny rule may
// list capabilities and filters, and applies only to the listed capability when
// its filter passes.
func TestFirewallDenyWithCapabilities(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/*"}, Effect: "deny", Capabilities: []CapabilityGrant{
			{Name: CapMRComment, Filter: CapabilityFilter{Require: []string{"do-not-touch"}}},
		}},
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: grants(CapMRRead, CapMRComment)},
	})

	if d := p.EvaluateWithTags("team/app", CapMRComment, TagSet{Known: true, Values: []string{"do-not-touch"}}); d.Allowed || d.Reason != "denied by rule" {
		t.Errorf("deny with matching filter = %+v, want denied by rule", d)
	}
	if d := p.EvaluateWithTags("team/app", CapMRComment, TagSet{Known: true, Values: []string{"ok"}}); !d.Allowed {
		t.Errorf("deny filter miss should fall through to the allow rule: %+v", d)
	}
	if d := p.EvaluateWithTags("team/app", CapMRRead, TagSet{Known: true, Values: []string{"do-not-touch"}}); !d.Allowed {
		t.Errorf("a capability the deny does not list is unaffected: %+v", d)
	}

	// A deny rule without capabilities still denies every capability in scope.
	q := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/*"}, Effect: "deny"},
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: grants(CapMRRead)},
	})
	if d := q.Evaluate("team/app", CapMRRead); d.Allowed || d.Reason != "denied by rule" {
		t.Errorf("scope-only deny = %+v, want denied by rule", d)
	}
}

// TestFirewallFilterFallThrough pins that an evaluated filter miss makes the
// rule fall through to a later rule, while an unevaluable filter fails closed.
func TestFirewallFilterFallThrough(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapRepoRead, Filter: CapabilityFilter{Paths: PathFilter{Include: []string{"docs/**"}}}},
		}},
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: grants(CapRepoRead)},
	})
	if d := p.EvaluateResource("team/app", CapRepoRead, TagSet{}, "docs/a.md"); !d.Allowed {
		t.Errorf("included path denied: %+v", d)
	}
	if d := p.EvaluateResource("team/app", CapRepoRead, TagSet{}, "src/a.go"); !d.Allowed {
		t.Errorf("path miss should fall through to the broad allow: %+v", d)
	}

	// Without a later rule the miss denies by default, reporting the miss.
	q := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapRepoRead, Filter: CapabilityFilter{Paths: PathFilter{Include: []string{"docs/**"}}}},
		}},
	})
	if d := q.EvaluateResource("team/app", CapRepoRead, TagSet{}, "src/a.go"); d.Allowed || d.Reason != "path not allowed" {
		t.Errorf("path miss without fallback = %+v, want denied (path not allowed)", d)
	}

	// Unknown tag information is not a filter miss: it fails closed and never
	// falls through to a broader rule.
	r := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapRepoRead, Filter: CapabilityFilter{Require: []string{"ai-ok"}}},
		}},
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: grants(CapRepoRead)},
	})
	if d := r.EvaluateWithTags("team/app", CapRepoRead, TagSet{}); d.Allowed || d.Reason != "tag information unavailable" {
		t.Errorf("unknown tags = %+v, want fail-closed (tag information unavailable)", d)
	}
}

// TestFirewallShadowing documents the security-critical rule order: a broad
// allow above a specific deny hides the deny (first-match-wins).
func TestFirewallShadowing(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: grants(CapMRComment)},
		{Repositories: []string{"team/secret"}, Effect: "deny"},
	})
	if d := p.Evaluate("team/secret", CapMRComment); !d.Allowed {
		t.Errorf("broad allow before deny should shadow the deny: %+v", d)
	}

	q := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/secret"}, Effect: "deny"},
		{Repositories: []string{"team/*"}, Effect: "allow", Capabilities: grants(CapMRComment)},
	})
	if d := q.Evaluate("team/secret", CapMRComment); d.Allowed || d.Reason != "denied by rule" {
		t.Errorf("deny first = %+v, want denied by rule", d)
	}
}

func TestEvaluatePreCheck(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapMRComment, Filter: CapabilityFilter{Require: []string{"ai-reviewed"}}},
		}},
		{Repositories: []string{"team/denied"}, Effect: "deny"},
		{Repositories: []string{"team/capped"}, Effect: "deny", Capabilities: grants(CapMRComment)},
		{Repositories: []string{"team/filtered-deny"}, Effect: "deny", Capabilities: []CapabilityGrant{
			{Name: CapMRComment, Filter: CapabilityFilter{Require: []string{"x"}}},
		}},
		{Repositories: []string{"team/filtered-deny"}, Effect: "allow", Capabilities: grants(CapMRComment)},
	})
	tests := []struct {
		repo        string
		c           Capability
		wantGranted bool
		wantMatched bool
	}{
		{"team/app", CapMRComment, true, true},
		{"team/app", CapMRRead, false, true},
		{"team/denied", CapMRComment, false, true},
		{"team/capped", CapMRComment, false, true},
		{"team/filtered-deny", CapMRComment, true, true},
		{"other/repo", CapMRComment, false, false},
	}
	for _, tt := range tests {
		granted, matched := p.EvaluatePreCheck(tt.repo, tt.c)
		if granted != tt.wantGranted || matched != tt.wantMatched {
			t.Errorf("EvaluatePreCheck(%q, %q) = (%v, %v), want (%v, %v)",
				tt.repo, tt.c, granted, matched, tt.wantGranted, tt.wantMatched)
		}
	}
}

func TestNoAIExemptAll(t *testing.T) {
	all := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapMRRead, Filter: CapabilityFilter{NoAIExempt: true}},
		}},
	})
	if !all.NoAIExemptAll("team/app", CapMRRead) {
		t.Error("single exempt grant: NoAIExemptAll = false, want true")
	}
	mixed := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapMRRead, Filter: CapabilityFilter{NoAIExempt: true, Require: []string{"a"}}},
		}},
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: grants(CapMRRead)},
	})
	if mixed.NoAIExemptAll("team/app", CapMRRead) {
		t.Error("mixed exemption: NoAIExemptAll = true, want false (conservative)")
	}
	if all.NoAIExemptAll("other/repo", CapMRRead) {
		t.Error("no matching grant: NoAIExemptAll = true, want false")
	}
}

// TestEvaluateWithTagsIgnoresLabelsDimension pins that a tags-only evaluation
// has no label context: the post-state re-check must be able to evaluate the
// grant's tag filter without re-evaluating the labels argument filter.
func TestEvaluateWithTagsIgnoresLabelsDimension(t *testing.T) {
	p := mustBuild(t, []RuleSpec{
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: []CapabilityGrant{
			{Name: CapMRLabel, Filter: CapabilityFilter{
				Require: []string{"ai-reviewed"},
				Labels:  LabelFilter{Include: []string{"ai-done"}},
			}},
		}},
	})
	if d := p.EvaluateWithTags("team/app", CapMRLabel, TagSet{Known: true, Values: []string{"ai-reviewed"}}); !d.Allowed {
		t.Errorf("tags-only evaluation = %+v, want the labels dimension ignored", d)
	}
}
