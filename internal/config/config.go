// Package config loads and validates the mcp-forj YAML configuration.
package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hvo/mcp-forj/internal/policy"
)

// envRefPattern matches a whole-string environment reference such as "${NAME}".
var envRefPattern = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)

const (
	defaultServerName     = "mcp-forj"
	defaultLogLevel       = "info"
	defaultMarkerFile     = ".noai"
	defaultRequestTimeout = 30 * time.Second
)

// Config is the root configuration.
type Config struct {
	// Server holds process-wide settings.
	Server ServerConfig `yaml:"server"`
	// Providers holds one entry per configured code hosting provider.
	Providers []ProviderConfig `yaml:"providers"`
}

// ServerConfig holds process-wide settings.
type ServerConfig struct {
	// Name is the server name advertised to MCP clients.
	Name string `yaml:"name"`
	// LogLevel is one of debug, info, warn or error.
	LogLevel string `yaml:"log_level"`
	// NoAI configures the marker-file guard.
	NoAI NoAIConfig `yaml:"noai"`
}

// NoAIConfig configures the .noai marker guard.
type NoAIConfig struct {
	// MarkerFile is the repository file that disables all access.
	MarkerFile string `yaml:"marker_file"`
}

// ProviderConfig describes one provider instance.
type ProviderConfig struct {
	// Name is the logical name used by tools.
	Name string `yaml:"name"`
	// Type selects the provider implementation, e.g. "gitlab".
	Type string `yaml:"type"`
	// BaseURL is the provider's API base URL.
	BaseURL string `yaml:"base_url"`
	// Token is the API token, either a literal secret or a whole-string
	// ${NAME} environment reference. It is resolved by Parse.
	Token Secret `yaml:"token"`
	// RequestTimeout bounds each provider request.
	RequestTimeout Duration `yaml:"request_timeout"`
	// Rules are the access rules, evaluated in order.
	Rules []RuleConfig `yaml:"rules"`
}

// RuleConfig is one configured access rule.
type RuleConfig struct {
	// Repositories are doublestar glob patterns.
	Repositories []string `yaml:"repositories"`
	// Effect is "allow" or "deny".
	Effect string `yaml:"effect"`
	// Capabilities are granted by an "allow" rule.
	Capabilities []string `yaml:"capabilities"`
}

// Secret is a configuration secret. Its formatting methods always redact the
// value, so accidental %v/%+v/%#v or slog dumps of a Config cannot leak it.
// The raw value is reachable only through Value.
type Secret string

// Value returns the raw secret. It is the only accessor.
func (s Secret) Value() string { return string(s) }

// String implements fmt.Stringer and always redacts the value.
func (s Secret) String() string { return "[REDACTED]" }

// GoString implements fmt.GoStringer and always redacts the value.
func (s Secret) GoString() string { return "[REDACTED]" }

// LogValue implements slog.LogValuer and always redacts the value.
func (s Secret) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// Duration is a time.Duration that unmarshals from a Go duration string such as
// "30s".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	return d.UnmarshalText([]byte(s))
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(text), err)
	}
	*d = Duration(parsed)
	return nil
}

// String implements fmt.Stringer.
func (d Duration) String() string {
	return time.Duration(d).String()
}

// Load reads, decodes and validates the configuration at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	return Parse(data)
}

// Parse decodes and validates configuration bytes.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	for i := range cfg.Providers {
		resolved, err := resolveSecret(string(cfg.Providers[i].Token))
		if err != nil {
			return nil, fmt.Errorf("config: provider %q: %w", cfg.Providers[i].Name, err)
		}
		cfg.Providers[i].Token = resolved
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// resolveSecret trims surrounding whitespace and resolves a whole-string
// ${NAME} environment reference. Any other non-empty value is used as a literal
// secret. Error messages name only the environment variable, never its value.
func resolveSecret(raw string) (Secret, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("token is required")
	}
	if envRefPattern.MatchString(trimmed) {
		name := trimmed[2 : len(trimmed)-1]
		value := os.Getenv(name)
		if value == "" {
			return "", fmt.Errorf("token: environment variable %s is empty or unset", name)
		}
		return Secret(value), nil
	}
	return Secret(trimmed), nil
}

// Validate checks the configuration for consistency. It fails closed: unknown
// values are rejected.
func (c *Config) Validate() error {
	switch c.Server.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: invalid log_level %q", c.Server.LogLevel)
	}

	if len(c.Providers) == 0 {
		return fmt.Errorf("config: at least one provider is required")
	}

	seen := make(map[string]bool, len(c.Providers))
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Name == "" {
			return fmt.Errorf("config: provider %d: name is required", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("config: duplicate provider name %q", p.Name)
		}
		seen[p.Name] = true

		if p.Type != "gitlab" {
			return fmt.Errorf("config: provider %q: unsupported type %q", p.Name, p.Type)
		}
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("config: provider %q: base_url must be an absolute http(s) URL", p.Name)
		}
		if p.Token.Value() == "" {
			return fmt.Errorf("config: provider %q: token is required", p.Name)
		}

		for j, rule := range p.Rules {
			if rule.Effect != string(policy.EffectAllow) && rule.Effect != string(policy.EffectDeny) {
				return fmt.Errorf("config: provider %q rule %d: effect must be %q or %q", p.Name, j, policy.EffectAllow, policy.EffectDeny)
			}
			if len(rule.Repositories) == 0 {
				return fmt.Errorf("config: provider %q rule %d: repositories must not be empty", p.Name, j)
			}
			for _, pattern := range rule.Repositories {
				if pattern == "" {
					return fmt.Errorf("config: provider %q rule %d: empty repository pattern", p.Name, j)
				}
			}
			for _, capability := range rule.Capabilities {
				if !policy.IsKnownCapability(capability) {
					return fmt.Errorf("config: provider %q rule %d: unknown capability %q", p.Name, j, capability)
				}
			}
		}
	}
	return nil
}

// ProviderByName returns the provider with the given logical name.
func (c *Config) ProviderByName(name string) (*ProviderConfig, bool) {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i], true
		}
	}
	return nil, false
}

func (c *Config) applyDefaults() {
	if c.Server.Name == "" {
		c.Server.Name = defaultServerName
	}
	if c.Server.LogLevel == "" {
		c.Server.LogLevel = defaultLogLevel
	}
	if c.Server.NoAI.MarkerFile == "" {
		c.Server.NoAI.MarkerFile = defaultMarkerFile
	}
	for i := range c.Providers {
		if c.Providers[i].RequestTimeout == 0 {
			c.Providers[i].RequestTimeout = Duration(defaultRequestTimeout)
		}
	}
}
