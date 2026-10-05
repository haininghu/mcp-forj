package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hvo/mcp-forj/internal/policy"
)

func tokenYAML(token string) string {
	return `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: "` + token + `"
    rules:
      - repositories: ["a/b"]
        effect: allow
        capabilities: [mr:read]
`
}

const validYAML = `
server:
  name: test-server
  log_level: debug
  noai:
    marker_file: .noai
providers:
  - name: gitlab-work
    type: gitlab
    base_url: https://gitlab.example.com
    token: "literal-secret"
    request_timeout: 45s
    rules:
      - repositories: ["team/service-a"]
        effect: allow
        capabilities: [mr:read, mr:comment]
      - repositories: ["legacy/**"]
        effect: deny
`

func TestParseValid(t *testing.T) {
	cfg, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Server.Name != "test-server" {
		t.Errorf("server name = %q, want test-server", cfg.Server.Name)
	}
	if cfg.Server.LogLevel != "debug" {
		t.Errorf("log level = %q, want debug", cfg.Server.LogLevel)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(cfg.Providers))
	}
	p := cfg.Providers[0]
	if p.RequestTimeout != Duration(45*time.Second) {
		t.Errorf("request timeout = %s, want 45s", p.RequestTimeout)
	}
	if got := p.Rules[0].Capabilities; len(got) != 2 || got[0].Name != "mr:read" || got[1].Name != "mr:comment" {
		t.Errorf("capabilities = %v", got)
	}
	if p.Token.Value() != "literal-secret" {
		t.Errorf("token = %q, want literal-secret", p.Token.Value())
	}
	if _, ok := cfg.ProviderByName("gitlab-work"); !ok {
		t.Error("ProviderByName did not find configured provider")
	}
	if _, ok := cfg.ProviderByName("missing"); ok {
		t.Error("ProviderByName found a missing provider")
	}
}

func TestParseDefaults(t *testing.T) {
	yaml := `
providers:
  - name: p
    type: gitlab
    base_url: http://gitlab.internal
    token: TOKEN
    rules:
      - repositories: ["a/b"]
        effect: allow
        capabilities: [mr:read]
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Server.Name != defaultServerName {
		t.Errorf("server name = %q, want %q", cfg.Server.Name, defaultServerName)
	}
	if cfg.Server.LogLevel != defaultLogLevel {
		t.Errorf("log level = %q, want %q", cfg.Server.LogLevel, defaultLogLevel)
	}
	if cfg.Server.NoAI.MarkerFile != defaultMarkerFile {
		t.Errorf("marker file = %q, want %q", cfg.Server.NoAI.MarkerFile, defaultMarkerFile)
	}
	if cfg.Providers[0].RequestTimeout != Duration(defaultRequestTimeout) {
		t.Errorf("request timeout = %s, want %s", cfg.Providers[0].RequestTimeout, defaultRequestTimeout)
	}
}

func TestDurationStringAndText(t *testing.T) {
	var d Duration
	if err := d.UnmarshalText([]byte("1m30s")); err != nil {
		t.Fatalf("UnmarshalText: %v", err)
	}
	if got := d.String(); got != "1m30s" {
		t.Errorf("String = %q, want 1m30s", got)
	}
	if err := d.UnmarshalText([]byte("nonsense")); err == nil {
		t.Error("UnmarshalText accepted invalid duration")
	}
}

func TestValidationFailures(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "unknown capability",
			yaml: `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules:
      - repositories: ["a/b"]
        effect: allow
        capabilities: [repo:teleport]
`,
			wantErr: "unknown capability",
		},
		{
			name: "missing token",
			yaml: `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    rules:
      - repositories: ["a/b"]
        effect: allow
        capabilities: [mr:read]
`,
			wantErr: "token is required",
		},
		{
			name: "duplicate provider name",
			yaml: `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules:
      - repositories: ["a/b"]
        effect: allow
        capabilities: [mr:read]
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules:
      - repositories: ["a/c"]
        effect: allow
        capabilities: [mr:read]
`,
			wantErr: "duplicate provider name",
		},
		{
			name: "bad effect",
			yaml: `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules:
      - repositories: ["a/b"]
        effect: maybe
`,
			wantErr: "effect must be",
		},
		{
			name: "bad log level",
			yaml: `
server:
  log_level: verbose
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules:
      - repositories: ["a/b"]
        effect: allow
        capabilities: [mr:read]
`,
			wantErr: "invalid log_level",
		},
		{
			name: "relative base_url",
			yaml: `
providers:
  - name: p
    type: gitlab
    base_url: gitlab.example.com
    token: T
    rules:
      - repositories: ["a/b"]
        effect: allow
        capabilities: [mr:read]
`,
			wantErr: "absolute http(s) URL",
		},
		{
			name: "unsupported type",
			yaml: `
providers:
  - name: p
    type: github
    base_url: https://example.com
    token: T
    rules:
      - repositories: ["a/b"]
        effect: allow
        capabilities: [mr:read]
`,
			wantErr: "unsupported type",
		},
		{
			name: "empty repositories",
			yaml: `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules:
      - effect: allow
        capabilities: [mr:read]
`,
			wantErr: "repositories must not be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatal("Parse succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestZeroProvidersRejected(t *testing.T) {
	_, err := Parse([]byte("server:\n  name: empty\n"))
	if err == nil {
		t.Fatal("Parse accepted a config with no providers")
	}
	if !strings.Contains(err.Error(), "at least one provider") {
		t.Errorf("error = %q, want mention of at least one provider", err)
	}
}

func TestEmptyRulesDenyByDefault(t *testing.T) {
	yaml := `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules: []
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse rejected empty rules: %v", err)
	}
	specs := make([]policy.RuleSpec, 0, len(cfg.Providers[0].Rules))
	for _, rule := range cfg.Providers[0].Rules {
		specs = append(specs, policy.RuleSpec{
			Repositories: rule.Repositories,
			Effect:       rule.Effect,
		})
	}
	pol, err := policy.Build(specs)
	if err != nil {
		t.Fatalf("policy.Build: %v", err)
	}
	decision := pol.Evaluate("team/app", policy.CapMRRead)
	if decision.Allowed || decision.Matched {
		t.Errorf("empty rules allowed access: %+v", decision)
	}
}

func TestTokenLiteral(t *testing.T) {
	cfg, err := Parse([]byte(tokenYAML("literal-token")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Providers[0].Token.Value(); got != "literal-token" {
		t.Errorf("token = %q, want literal-token", got)
	}
}

func TestTokenLiteralWithDollarStaysLiteral(t *testing.T) {
	cfg, err := Parse([]byte(tokenYAML("abc$def")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Providers[0].Token.Value(); got != "abc$def" {
		t.Errorf("token = %q, want abc$def", got)
	}
}

func TestTokenEnvReference(t *testing.T) {
	t.Setenv("MCP_FORJ_TEST_TOKEN", "resolved-secret")
	cfg, err := Parse([]byte(tokenYAML("${MCP_FORJ_TEST_TOKEN}")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Providers[0].Token.Value(); got != "resolved-secret" {
		t.Errorf("token = %q, want resolved-secret", got)
	}
}

func TestTokenEnvUnsetRejected(t *testing.T) {
	_, err := Parse([]byte(tokenYAML("${MCP_FORJ_TEST_UNSET_XYZ}")))
	if err == nil {
		t.Fatal("Parse accepted an unset environment reference")
	}
	if !strings.Contains(err.Error(), "MCP_FORJ_TEST_UNSET_XYZ") {
		t.Errorf("error = %q, want it to name the environment variable", err)
	}
}

func TestTokenEnvEmptyRejected(t *testing.T) {
	t.Setenv("MCP_FORJ_TEST_EMPTY", "")
	_, err := Parse([]byte(tokenYAML("${MCP_FORJ_TEST_EMPTY}")))
	if err == nil {
		t.Fatal("Parse accepted an empty environment reference")
	}
	if !strings.Contains(err.Error(), "MCP_FORJ_TEST_EMPTY") {
		t.Errorf("error = %q, want it to name the environment variable", err)
	}
}

func TestTokenWhitespaceTrimmed(t *testing.T) {
	cfg, err := Parse([]byte(tokenYAML("  spaced-token  ")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Providers[0].Token.Value(); got != "spaced-token" {
		t.Errorf("token = %q, want spaced-token", got)
	}
}

func TestTokenRedaction(t *testing.T) {
	const secret = "top-secret-value"
	t.Setenv("MCP_FORJ_TEST_SECRET", secret)
	cfg, err := Parse([]byte(tokenYAML("${MCP_FORJ_TEST_SECRET}")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Providers[0].Token.Value() != secret {
		t.Fatalf("token not resolved")
	}
	for _, formatted := range []string{
		fmt.Sprintf("%v", cfg),
		fmt.Sprintf("%+v", cfg),
		fmt.Sprintf("%#v", cfg),
		cfg.Providers[0].Token.GoString(),
		cfg.Providers[0].Token.String(),
	} {
		if strings.Contains(formatted, secret) {
			t.Errorf("secret leaked in %q", formatted)
		}
	}
	if cfg.Providers[0].Token.String() != "[REDACTED]" {
		t.Errorf("Token.String() = %q, want [REDACTED]", cfg.Providers[0].Token.String())
	}
	if cfg.Providers[0].Token.GoString() != "[REDACTED]" {
		t.Errorf("Token.GoString() = %q, want [REDACTED]", cfg.Providers[0].Token.GoString())
	}

	jsonBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(jsonBytes), secret) {
		t.Errorf("json.Marshal leaked secret: %s", jsonBytes)
	}

	yamlBytes, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if strings.Contains(string(yamlBytes), secret) {
		t.Errorf("yaml.Marshal leaked secret: %s", yamlBytes)
	}
}

func TestTokenEnvWhitespaceOnlyRejected(t *testing.T) {
	t.Setenv("MCP_FORJ_TEST_BLANK", "   ")
	_, err := Parse([]byte(tokenYAML("${MCP_FORJ_TEST_BLANK}")))
	if err == nil {
		t.Fatal("Parse accepted a whitespace-only environment reference")
	}
	if !strings.Contains(err.Error(), "MCP_FORJ_TEST_BLANK") {
		t.Errorf("error = %q, want it to name the environment variable", err)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	yaml := `
server:
  bogus: true
`
	if _, err := Parse([]byte(yaml)); err == nil {
		t.Fatal("Parse accepted an unknown field")
	}
}

// configWithCaps wraps a capabilities block into a valid provider rule.
func configWithCaps(caps string) string {
	return `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules:
      - repositories: ["a/b"]
        effect: allow
` + caps
}

func TestCapabilityGrantScalarForm(t *testing.T) {
	cfg, err := Parse([]byte(configWithCaps("        capabilities: [mr:read, mr:comment]\n")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := cfg.Providers[0].Rules[0].Capabilities
	if len(got) != 2 || got[0].Name != "mr:read" || got[1].Name != "mr:comment" {
		t.Fatalf("capabilities = %+v", got)
	}
	if len(got[0].Require) != 0 || len(got[0].Exclude) != 0 {
		t.Errorf("scalar capability carries a filter: %+v", got[0])
	}
}

func TestCapabilityGrantCompactForm(t *testing.T) {
	yaml := `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules:
      - repositories: ["a/b"]
        effect: allow
        capabilities:
          - mr:read
          - mr:comment:
              require: [ai-reviewed]
              exclude: [do-not-touch]
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := cfg.Providers[0].Rules[0].Capabilities
	if len(got) != 2 {
		t.Fatalf("capabilities = %+v", got)
	}
	if got[0].Name != "mr:read" || len(got[0].Require) != 0 {
		t.Errorf("grant[0] = %+v, want plain mr:read", got[0])
	}
	if got[1].Name != "mr:comment" {
		t.Fatalf("grant[1].Name = %q, want mr:comment", got[1].Name)
	}
	if len(got[1].Require) != 1 || got[1].Require[0] != "ai-reviewed" {
		t.Errorf("grant[1].Require = %v", got[1].Require)
	}
	if len(got[1].Exclude) != 1 || got[1].Exclude[0] != "do-not-touch" {
		t.Errorf("grant[1].Exclude = %v", got[1].Exclude)
	}
}

func TestCapabilityGrantRepoFilterAccepted(t *testing.T) {
	yaml := `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules:
      - repositories: ["a/b"]
        effect: allow
        capabilities:
          - repo:read:
              require: [ai-ok]
          - repo:list:
              exclude: [confidential]
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse rejected a repo capability filter: %v", err)
	}
	got := cfg.Providers[0].Rules[0].Capabilities
	if len(got) != 2 {
		t.Fatalf("capabilities = %+v", got)
	}
	if got[0].Name != "repo:read" || len(got[0].Require) != 1 || got[0].Require[0] != "ai-ok" {
		t.Errorf("grant[0] = %+v", got[0])
	}
	if got[1].Name != "repo:list" || len(got[1].Exclude) != 1 || got[1].Exclude[0] != "confidential" {
		t.Errorf("grant[1] = %+v", got[1])
	}
}

func TestCapabilityGrantNullValue(t *testing.T) {
	yaml := `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules:
      - repositories: ["a/b"]
        effect: allow
        capabilities:
          - mr:comment:
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	grant := cfg.Providers[0].Rules[0].Capabilities[0]
	if grant.Name != "mr:comment" || len(grant.Require) != 0 || len(grant.Exclude) != 0 {
		t.Fatalf("grant = %+v, want plain mr:comment", grant)
	}
}

func TestCapabilityGrantRejections(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "unknown filter key",
			yaml: `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules:
      - repositories: ["a/b"]
        effect: allow
        capabilities:
          - mr:comment:
              bogus: [x]
`,
			wantErr: "unknown filter key",
		},
		{
			name: "more than one capability key",
			yaml: `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules:
      - repositories: ["a/b"]
        effect: allow
        capabilities:
          - mr:read: null
            mr:comment: null
`,
			wantErr: "exactly one capability key",
		},
		{
			name: "duplicate filter key",
			yaml: `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules:
      - repositories: ["a/b"]
        effect: allow
        capabilities:
          - mr:comment:
              require: [a]
              require: [b]
`,
			wantErr: "duplicate filter key",
		},
		{
			name: "tag in both lists",
			yaml: `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules:
      - repositories: ["a/b"]
        effect: allow
        capabilities:
          - mr:comment:
              require: [ai-reviewed]
              exclude: [ai-reviewed]
`,
			wantErr: "both require and exclude",
		},
		{
			name: "empty tag",
			yaml: `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules:
      - repositories: ["a/b"]
        effect: allow
        capabilities:
          - mr:comment:
              require: ["   "]
`,
			wantErr: "tag must not be empty",
		},
		{
			name: "duplicate capability",
			yaml: `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules:
      - repositories: ["a/b"]
        effect: allow
        capabilities: [mr:read, mr:read]
`,
			wantErr: "duplicate capability",
		},
		{
			name: "filter on deny rule",
			yaml: `
providers:
  - name: p
    type: gitlab
    base_url: https://example.com
    token: T
    rules:
      - repositories: ["a/b"]
        effect: deny
        capabilities:
          - mr:comment:
              require: [x]
`,
			wantErr: "not allowed on deny rules",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatal("Parse succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}
