package storage

import (
	"runtime"
	"sync"
	"time"
)

// Manager defines the interface for storage operations
type Manager interface {
	// CreateHook creates a new hook and returns it
	CreateHook(domain string) *Hook

	// GetHook retrieves a hook by ID
	GetHook(id string) (*Hook, bool)

	// AddInteraction adds an interaction to a hook
	AddInteraction(hookID string, interaction *Interaction) error

	// PollInteractions retrieves and deletes interactions for a hook
	PollInteractions(hookID string) ([]*Interaction, error)

	// GetAllHooks returns all registered hooks
	GetAllHooks() []*Hook

	// GetAllInteractions returns all interactions (for eviction purposes)
	GetAllInteractions() map[string][]*Interaction

	// DeleteInteractions deletes specific interactions from a hook
	DeleteInteractions(hookID string, interactionIDs []string)

	// DeleteHook deletes a hook and all its interactions
	DeleteHook(hookID string)

	// Stats returns storage statistics
	Stats() Stats
}

// Stats represents storage statistics
type Stats struct {
	HooksActive       int
	InteractionsTotal int
	InteractionsDNS   int
	InteractionsHTTP  int
	Memory            MemoryStats
}

// MemoryManager implements in-memory storage
type MemoryManager struct {
	hooks        map[string]*Hook
	interactions map[string][]*Interaction
	mu           sync.RWMutex
	idGenerator  func() string
}

// NewMemoryManager creates a new in-memory storage manager
func NewMemoryManager(idGenerator func() string) *MemoryManager {
	return &MemoryManager{
		hooks:        make(map[string]*Hook),
		interactions: make(map[string][]*Interaction),
		idGenerator:  idGenerator,
	}
}

// CreateHook creates a new hook
func (m *MemoryManager) CreateHook(domain string) *Hook {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := m.idGenerator()
	hook := &Hook{
		ID:        id,
		DNS:       id + "." + domain,
		HTTP:      "http://" + id + "." + domain,
		HTTPS:     "https://" + id + "." + domain,
		CreatedAt: time.Now().UTC(),
	}

	m.hooks[id] = hook
	m.interactions[id] = make([]*Interaction, 0)

	return hook
}

// GetHook retrieves a hook by ID
func (m *MemoryManager) GetHook(id string) (*Hook, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	hook, exists := m.hooks[id]
	return hook, exists
}

// AddInteraction adds an interaction to a hook
func (m *MemoryManager) AddInteraction(hookID string, interaction *Interaction) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if hook exists
	if _, exists := m.hooks[hookID]; !exists {
		return nil // Silently ignore interactions for non-existent hooks
	}

	m.interactions[hookID] = append(m.interactions[hookID], interaction)
	return nil
}

// PollInteractions retrieves and deletes interactions for a hook
func (m *MemoryManager) PollInteractions(hookID string) ([]*Interaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	interactions, exists := m.interactions[hookID]
	if !exists {
		return []*Interaction{}, nil
	}

	// Return a copy and clear the slice
	result := make([]*Interaction, len(interactions))
	copy(result, interactions)

	m.interactions[hookID] = make([]*Interaction, 0)

	return result, nil
}

// GetAllHooks returns all registered hooks
func (m *MemoryManager) GetAllHooks() []*Hook {
	m.mu.RLock()
	defer m.mu.RUnlock()

	hooks := make([]*Hook, 0, len(m.hooks))
	for _, hook := range m.hooks {
		hooks = append(hooks, hook)
	}

	return hooks
}

// GetAllInteractions returns all interactions
func (m *MemoryManager) GetAllInteractions() map[string][]*Interaction {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Return a shallow copy
	result := make(map[string][]*Interaction, len(m.interactions))
	for k, v := range m.interactions {
		result[k] = v
	}

	return result
}

// DeleteInteractions deletes specific interactions from a hook
func (m *MemoryManager) DeleteInteractions(hookID string, interactionIDs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	interactions, exists := m.interactions[hookID]
	if !exists {
		return
	}

	// Create a set of IDs to delete
	toDelete := make(map[string]bool)
	for _, id := range interactionIDs {
		toDelete[id] = true
	}

	// Filter out interactions
	filtered := make([]*Interaction, 0)
	for _, interaction := range interactions {
		if !toDelete[interaction.ID] {
			filtered = append(filtered, interaction)
		}
	}

	m.interactions[hookID] = filtered
}

// DeleteHook deletes a hook and all its interactions
func (m *MemoryManager) DeleteHook(hookID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.hooks, hookID)
	delete(m.interactions, hookID)
}

// Stats returns storage statistics
func (m *MemoryManager) Stats() Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := Stats{
		HooksActive: len(m.hooks),
	}

	for _, interactions := range m.interactions {
		stats.InteractionsTotal += len(interactions)
		for _, interaction := range interactions {
			switch interaction.Type {
			case InteractionTypeDNS:
				stats.InteractionsDNS++
			case InteractionTypeHTTP:
				stats.InteractionsHTTP++
			}
		}
	}

	// Get detailed memory usage from Go runtime
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	// Populate detailed memory stats
	stats.Memory = MemoryStats{
		AllocMB:     int(memStats.Alloc / (1024 * 1024)),
		HeapInuseMB: int(memStats.HeapInuse / (1024 * 1024)),
		SysMB:       int(memStats.Sys / (1024 * 1024)),
		GCRuns:      memStats.NumGC,
	}

	return stats
}
