package provider

import (
	"maps"
	"sort"
	"sync"
)

// Registry holds providers keyed by their logical name. It is safe for
// concurrent use.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]Provider)}
}

// Register adds or replaces a provider.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.Name()] = p
}

// Get returns the provider with the given name.
func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// Names returns the registered provider names in sorted order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// All returns a copy of the provider map.
func (r *Registry) All() map[string]Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Provider, len(r.providers))
	maps.Copy(out, r.providers)
	return out
}
