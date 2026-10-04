package provider

import (
	"fmt"

	"github.com/hvo/mcp-forj/internal/config"
)

// Factory constructs a Provider from its configuration.
type Factory func(cfg config.ProviderConfig) (Provider, error)

// factories is populated by provider implementations via RegisterFactory. This
// indirection lets this package expose New without importing the concrete
// provider packages, which themselves import provider (avoiding an import
// cycle).
var factories = map[string]Factory{}

// RegisterFactory registers a constructor for a provider type. Implementations
// call it from an init function; the provider package must be imported for its
// side effects.
func RegisterFactory(providerType string, f Factory) {
	if f == nil {
		panic("provider: nil factory")
	}
	factories[providerType] = f
}

// New constructs the provider described by cfg, using the factory registered
// for cfg.Type.
func New(cfg config.ProviderConfig) (Provider, error) {
	f, ok := factories[cfg.Type]
	if !ok {
		return nil, fmt.Errorf("provider: unsupported type %q", cfg.Type)
	}
	return f(cfg)
}
