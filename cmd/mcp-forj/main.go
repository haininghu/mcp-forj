// Command mcp-forj runs the policy-governed MCP server over stdio.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hvo/mcp-forj/internal/config"
	"github.com/hvo/mcp-forj/internal/policy"
	"github.com/hvo/mcp-forj/internal/provider"
	_ "github.com/hvo/mcp-forj/internal/provider/gitlab" // register the gitlab provider factory
	"github.com/hvo/mcp-forj/internal/server"
)

// version is injected at build time via -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-forj:", err)
		os.Exit(1)
	}
}

func run() error {
	defaultConfig := "configs/config.yaml"
	if fromEnv := os.Getenv("MCP_FORJ_CONFIG"); fromEnv != "" {
		defaultConfig = fromEnv
	}
	configPath := flag.String("config", defaultConfig, "path to the YAML configuration file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger := newLogger(cfg.Server.LogLevel)

	registry := provider.NewRegistry()
	policies := make(map[string]*policy.Policy, len(cfg.Providers))
	checkers := make(map[string]policy.FileChecker, len(cfg.Providers))
	for i := range cfg.Providers {
		pc := &cfg.Providers[i]
		p, err := provider.New(*pc)
		if err != nil {
			return fmt.Errorf("provider %q: %w", pc.Name, err)
		}
		registry.Register(p)
		checkers[pc.Name] = p

		specs := make([]policy.RuleSpec, len(pc.Rules))
		for j, rule := range pc.Rules {
			caps := make([]policy.CapabilityGrant, len(rule.Capabilities))
			for k, grant := range rule.Capabilities {
				caps[k] = policy.CapabilityGrant{
					Name: policy.Capability(grant.Name),
					Filter: policy.CapabilityFilter{
						Require: grant.Require,
						Exclude: grant.Exclude,
						Paths: policy.PathFilter{
							Include: grant.Paths.Include,
							Exclude: grant.Paths.Exclude,
						},
					},
				}
			}
			specs[j] = policy.RuleSpec{
				Repositories: rule.Repositories,
				Effect:       rule.Effect,
				Capabilities: caps,
			}
		}
		pol, err := policy.Build(specs)
		if err != nil {
			return fmt.Errorf("provider %q: %w", pc.Name, err)
		}
		policies[pc.Name] = pol
	}

	guard := policy.NewGuard(policies, checkers, cfg.Server.NoAI.MarkerFile, logger)
	srv := server.New(guard, registry, logger)

	logger.Info("starting mcp-forj",
		"version", version,
		"config", *configPath,
		"providers", registry.Names(),
	)

	if err := srv.MCPServer(version).Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
