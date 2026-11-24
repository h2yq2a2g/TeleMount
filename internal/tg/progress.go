package tg

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type Transfer struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"` // "upload" or "download"
	Name      string  `json:"name"`
	Done      int64   `json:"done"`
	Total     int64   `json:"total"`
	StartedAt int64   `json:"started_at"`
	UpdatedAt int64   `json:"updated_at"`
	SpeedBps  float64 `json:"speed_bps"`
}

type FloodWaitEvent struct {
	Method      string `json:"method"`
	DurationSec int    `json:"duration_sec"`
	At          int64  `json:"at"`
}

type UserInfo struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
	Phone     string `json:"phone"`
}

type StatusSnapshot struct {
	Connected       bool             `json:"connected"`
	ConnectionState string           `json:"connection_state"`
	Authorized      bool             `json:"authorized"`
	User            *UserInfo        `json:"user,omitempty"`
	ActiveTransfers []Transfer       `json:"active_transfers"`
	RecentFloods    []FloodWaitEvent `json:"recent_floods"`
	LastError       string           `json:"last_error,omitempty"`
}

type Tracker struct {
	mu          sync.RWMutex
	seq         atomic.Uint64
	transfers   map[string]*Transfer
	floods      []FloodWaitEvent
	connState   string
	connected   bool
	authorized  bool
	user        *UserInfo
	lastError   string
}

func NewTracker() *Tracker {
	return &Tracker{
		transfers: make(map[string]*Transfer),
		connState: "disconnected",
	}
}

func (t *Tracker) StartTransfer(kind, name string, total int64) string {
	id := fmt.Sprintf("%s-%d", kind, t.seq.Add(1))
	now := time.Now().Unix()
	t.mu.Lock()
	t.transfers[id] = &Transfer{
		ID:        id,
		Kind:      kind,
		Name:      name,
		Total:     total,
		StartedAt: now,
		UpdatedAt: now,
	}
	t.mu.Unlock()
	return id
}

func (t *Tracker) UpdateTransfer(id string, done int64) {
	now := time.Now()
	t.mu.Lock()
	if tr, ok := t.transfers[id]; ok {
		tr.Done = done
		tr.UpdatedAt = now.Unix()
		elapsed := float64(now.Unix() - tr.StartedAt)
		if elapsed < 1 {
			elapsed = 1
		}
		tr.SpeedBps = float64(done) / elapsed
	}
	t.mu.Unlock()
}

func (t *Tracker) AddTransferBytes(id string, delta int64) {
	now := time.Now()
	t.mu.Lock()
	if tr, ok := t.transfers[id]; ok {
		tr.Done += delta
		tr.UpdatedAt = now.Unix()
		elapsed := float64(now.Unix() - tr.StartedAt)
		if elapsed < 1 {
			elapsed = 1
		}
		tr.SpeedBps = float64(tr.Done) / elapsed
	}
	t.mu.Unlock()
}

func (t *Tracker) EndTransfer(id string) {
	t.mu.Lock()
	delete(t.transfers, id)
	t.mu.Unlock()
}

func (t *Tracker) RecordFloodWait(method string, d time.Duration) {
	ev := FloodWaitEvent{
		Method:      method,
		DurationSec: int(d.Seconds()),
		At:          time.Now().Unix(),
	}
	t.mu.Lock()
	t.floods = append([]FloodWaitEvent{ev}, t.floods...)
	if len(t.floods) > 20 {
		t.floods = t.floods[:20]
	}
	t.mu.Unlock()
}

func (t *Tracker) SetConnectionState(state string, connected bool) {
	t.mu.Lock()
	t.connState = state
	t.connected = connected
	t.mu.Unlock()
}

func (t *Tracker) SetAuthorized(ok bool, u *UserInfo) {
	t.mu.Lock()
	t.authorized = ok
	if ok && u != nil {
		cp := *u
		t.user = &cp
	} else if !ok {
		t.user = nil
	}
	t.mu.Unlock()
}

func (t *Tracker) SetLastError(err string) {
	t.mu.Lock()
	t.lastError = err
	t.mu.Unlock()
}

func (t *Tracker) Snapshot() StatusSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	active := make([]Transfer, 0, len(t.transfers))
	for _, tr := range t.transfers {
		active = append(active, *tr)
	}
	floods := make([]FloodWaitEvent, len(t.floods))
	copy(floods, t.floods)
	var u *UserInfo
	if t.user != nil {
		cp := *t.user
		u = &cp
	}
	return StatusSnapshot{
		Connected:       t.connected,
		ConnectionState: t.connState,
		Authorized:      t.authorized,
		User:            u,
		ActiveTransfers: active,
		RecentFloods:    floods,
		LastError:       t.lastError,
	}
}
