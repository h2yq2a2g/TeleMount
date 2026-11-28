package tg

import (
	"context"
	"fmt"
	"sync"
)

// Pool manages backup Telegram MTProto sessions alongside the primary account.
type Pool struct {
	dataDir string

	mu          sync.RWMutex
	managers    map[string]*Manager
	testBackends map[string]Backend
}

func NewPool(dataDir string) *Pool {
	return &Pool{
		dataDir:      dataDir,
		managers:     make(map[string]*Manager),
		testBackends: make(map[string]Backend),
	}
}

// IsValidAccountID checks that an account ID is a safe filename token (alphanumeric, '-', '_', 1..32 chars).
func IsValidAccountID(id string) bool {
	if len(id) == 0 || len(id) > 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (p *Pool) GetOrCreateManager(id string) *Manager {
	if !IsValidAccountID(id) {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.managers[id]; ok {
		return m
	}
	m := NewManagerWithSession(p.dataDir, fmt.Sprintf("tg_session_%s.json", id))
	p.managers[id] = m
	return m
}

func (p *Pool) GetManager(id string) (*Manager, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	m, ok := p.managers[id]
	return m, ok
}

func (p *Pool) GetBackend(id string) (Backend, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if b, ok := p.testBackends[id]; ok {
		return b, true
	}
	if m, ok := p.managers[id]; ok {
		return m, true
	}
	return nil, false
}

func (p *Pool) SetTestBackend(id string, b Backend) {
	p.mu.Lock()
	p.testBackends[id] = b
	p.mu.Unlock()
}

func (p *Pool) Remove(ctx context.Context, id string, logoutRemote bool) {
	p.mu.Lock()
	m, ok := p.managers[id]
	delete(p.managers, id)
	delete(p.testBackends, id)
	p.mu.Unlock()
	if ok {
		if logoutRemote {
			_ = m.Logout(ctx)
		} else {
			m.Stop()
		}
	}
}

func (p *Pool) StopAll() {
	p.mu.Lock()
	list := make([]*Manager, 0, len(p.managers))
	for _, m := range p.managers {
		list = append(list, m)
	}
	p.mu.Unlock()
	for _, m := range list {
		m.Stop()
	}
}
