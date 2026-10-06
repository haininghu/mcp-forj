package provider

import (
	"fmt"
	"sync"

	"github.com/hvo/mcp-forj/internal/config"
)

// Factory constructs a Provider from its configuration.
type Factory func(cfg config.ProviderConfig) (Provider, error)

// factories are populated by provider implementations via RegisterFactory. This
// indirection lets this package expose New without importing the concrete
// provider packages, which themselves import provider (avoiding an import
// cycle).
var (
	factoriesMu sync.RWMutex
	factories   = map[string]Factory{}
)

// RegisterFactory registers a constructor for a provider type. Implementations
// call it from an init function; the provider package must be imported for its
// side effects.
func RegisterFactory(providerType string, f Factory) {
	if f == nil {
		panic("provider: nil factory")
	}
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	factories[providerType] = f
}

// New constructs the provider described by cfg, using the factory registered
// for cfg.Type.
func New(cfg config.ProviderConfig) (Provider, error) {
	factoriesMu.RLock()
	f, ok := factories[cfg.Type]
	factoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("provider: unsupported type %q", cfg.Type)
	}
	return f(cfg)
}
