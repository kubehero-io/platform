// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package alerts

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Store errors.
var (
	ErrNotFound  = errors.New("not found")
	ErrNameTaken = errors.New("an alert rule with this name already exists")
	ErrTooMany   = errors.New("too many alert rules")
)

// Notification is one delivery attempt (channel stored redacted).
type Notification struct {
	At      time.Time
	AlertID string
	RuleID  string
	Event   string // firing | resolved | renotify | heartbeat
	Channel string
	OK      bool
	Error   string
}

// Store persists rules, alert state, silences and the notification log.
type Store interface {
	ListRules(ctx context.Context) ([]*Rule, error)
	GetRule(ctx context.Context, id string) (*Rule, error)
	// UpsertRule creates (empty ID) or replaces a rule, keeping
	// CreatedAt/CreatedBy on update.
	UpsertRule(ctx context.Context, r *Rule) (*Rule, error)
	DeleteRule(ctx context.Context, id string) error

	RuleAlerts(ctx context.Context, ruleID string) ([]*Alert, error)
	SaveAlerts(ctx context.Context, ruleID string, upserts []*Alert, deleted []string) error
	ListAlerts(ctx context.Context) ([]*Alert, error)

	CreateSilence(ctx context.Context, s *Silence) (*Silence, error)
	ListSilences(ctx context.Context, includeExpired bool, now time.Time) ([]*Silence, error)
	ExpireSilence(ctx context.Context, id string, now time.Time) error

	RecordNotifications(ctx context.Context, ns []Notification) error
	// SeedDefaults inserts rules once, on the first boot that finds no
	// rules; it reports whether it did.
	SeedDefaults(ctx context.Context, rules []*Rule) (bool, error)
	// Persistent is false for the in-memory store.
	Persistent() bool
}

// MemoryStore keeps everything in process memory — used when Postgres
// is not configured. Nothing survives a restart.
type MemoryStore struct {
	mu            sync.Mutex
	rules         map[string]*Rule
	alerts        map[string]map[string]*Alert // rule id → alert id → alert
	silences      map[string]*Silence
	notifications []Notification
	seeded        bool
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rules: map[string]*Rule{}, alerts: map[string]map[string]*Alert{}, silences: map[string]*Silence{}}
}

const (
	maxMemoryAlerts        = 50_000
	maxMemoryNotifications = 1000
)

func (m *MemoryStore) Persistent() bool { return false }

func (m *MemoryStore) ListRules(context.Context) ([]*Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Rule, 0, len(m.rules))
	for _, r := range m.rules {
		out = append(out, r.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *MemoryStore) GetRule(_ context.Context, id string) (*Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rules[id]
	if !ok {
		return nil, ErrNotFound
	}
	return r.Clone(), nil
}

func (m *MemoryStore) UpsertRule(_ context.Context, r *Rule) (*Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, other := range m.rules {
		if other.Name == r.Name && id != r.ID {
			return nil, ErrNameTaken
		}
	}
	now := time.Now().UTC()
	c := r.Clone()
	if c.ID == "" {
		if len(m.rules) >= maxRules {
			return nil, ErrTooMany
		}
		c.ID = uuid.NewString()
		c.CreatedAt = now
	} else {
		old, ok := m.rules[c.ID]
		if !ok {
			return nil, ErrNotFound
		}
		c.CreatedAt, c.CreatedBy = old.CreatedAt, old.CreatedBy
	}
	c.UpdatedAt = now
	m.rules[c.ID] = c
	return c.Clone(), nil
}

func (m *MemoryStore) DeleteRule(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rules[id]; !ok {
		return ErrNotFound
	}
	delete(m.rules, id)
	delete(m.alerts, id)
	return nil
}

func cloneAlert(a *Alert) *Alert {
	c := *a
	c.Labels = cloneMap(a.Labels)
	return &c
}

func (m *MemoryStore) RuleAlerts(_ context.Context, ruleID string) ([]*Alert, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Alert
	for _, a := range m.alerts[ruleID] {
		out = append(out, cloneAlert(a))
	}
	return out, nil
}

func (m *MemoryStore) SaveAlerts(_ context.Context, ruleID string, upserts []*Alert, deleted []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rules[ruleID]; !ok {
		return nil // rule deleted mid-evaluation
	}
	byID := m.alerts[ruleID]
	if byID == nil {
		byID = map[string]*Alert{}
		m.alerts[ruleID] = byID
	}
	for _, id := range deleted {
		delete(byID, id)
	}
	total := 0
	for _, as := range m.alerts {
		total += len(as)
	}
	for _, a := range upserts {
		if _, exists := byID[a.ID]; !exists && total >= maxMemoryAlerts {
			continue // bounded: drop new series rather than grow forever
		}
		byID[a.ID] = cloneAlert(a)
		total++
	}
	return nil
}

func (m *MemoryStore) ListAlerts(context.Context) ([]*Alert, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Alert
	for _, as := range m.alerts {
		for _, a := range as {
			out = append(out, cloneAlert(a))
		}
	}
	return out, nil
}

func (m *MemoryStore) CreateSilence(_ context.Context, s *Silence) (*Silence, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.silences) >= 10_000 {
		return nil, errors.New("too many silences")
	}
	c := *s
	c.ID = uuid.NewString()
	c.Matchers = cloneMap(s.Matchers)
	c.CreatedAt = time.Now().UTC()
	m.silences[c.ID] = &c
	out := c
	return &out, nil
}

func (m *MemoryStore) ListSilences(_ context.Context, includeExpired bool, now time.Time) ([]*Silence, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Silence
	for _, s := range m.silences {
		if !includeExpired && !s.EndsAt.After(now) {
			continue
		}
		c := *s
		c.Matchers = cloneMap(s.Matchers)
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EndsAt.After(out[j].EndsAt) })
	return out, nil
}

func (m *MemoryStore) ExpireSilence(_ context.Context, id string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.silences[id]
	if !ok {
		return ErrNotFound
	}
	if s.EndsAt.After(now) {
		s.EndsAt = now
		if !s.EndsAt.After(s.StartsAt) {
			s.StartsAt = now.Add(-time.Second)
		}
	}
	return nil
}

func (m *MemoryStore) RecordNotifications(_ context.Context, ns []Notification) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifications = append(m.notifications, ns...)
	if over := len(m.notifications) - maxMemoryNotifications; over > 0 {
		m.notifications = append([]Notification(nil), m.notifications[over:]...)
	}
	return nil
}

// Notifications returns the in-memory log (tests, debugging).
func (m *MemoryStore) Notifications() []Notification {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Notification(nil), m.notifications...)
}

func (m *MemoryStore) SeedDefaults(ctx context.Context, rules []*Rule) (bool, error) {
	m.mu.Lock()
	if m.seeded || len(m.rules) > 0 {
		m.seeded = true
		m.mu.Unlock()
		return false, nil
	}
	m.seeded = true
	m.mu.Unlock()
	for _, r := range rules {
		if _, err := m.UpsertRule(ctx, r); err != nil {
			return false, err
		}
	}
	return true, nil
}
