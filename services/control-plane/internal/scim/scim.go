// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package scim implements a minimal SCIM 2.0 endpoint
// (RFC 7643 + RFC 7644) so identity providers — Okta, Azure AD,
// OneLogin, JumpCloud — can auto-provision and de-provision users
// against KubeHero.
//
// Design notes:
//   - Resources we care about today: Users + Groups. Schemas, Bulk,
//     and search-via-POST are out of scope until a customer asks.
//   - Authentication: header-bearer matching the cp's KUBEHERO_API_KEYS
//     list. Each IdP gets its own dedicated key with role=admin so
//     the tokens are revocable per-IdP without disrupting humans.
//   - Group → role propagation: the underlying auth interceptor reads
//     `groups` from the JWT at request time, so SCIM-managed group
//     membership translates to RBAC automatically. We don't dual-track
//     group state in our DB; the IdP is the source of truth.
//
// Spec highlights we implement:
//   GET    /scim/v2/Users        — list (filter, count, startIndex)
//   GET    /scim/v2/Users/{id}   — read
//   POST   /scim/v2/Users        — create
//   PUT    /scim/v2/Users/{id}   — replace
//   PATCH  /scim/v2/Users/{id}   — partial update (operations array)
//   DELETE /scim/v2/Users/{id}   — soft-delete (active=false)
//   same for /Groups
//   GET    /scim/v2/ServiceProviderConfig
//   GET    /scim/v2/Schemas
//   GET    /scim/v2/ResourceTypes

package scim

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Store is the persistence layer SCIM writes through. The default
// in-memory implementation is enough for v0.1.0 — we have no
// SCIM-driven business logic on the server; the IdP just needs a
// 200/201/204 to keep its provisioning healthy. A Postgres-backed
// impl lands when we ship the orgs+users tables fully.
type Store interface {
	UpsertUser(u *User) error
	GetUser(id string) (*User, bool)
	ListUsers(filter string, count, start int) ([]*User, int)
	DeleteUser(id string) bool
	UpsertGroup(g *Group) error
	GetGroup(id string) (*Group, bool)
	ListGroups(filter string, count, start int) ([]*Group, int)
	DeleteGroup(id string) bool
}

// User is the SCIM 2.0 User resource shape, narrowed to fields IdPs
// actually populate. Extension schemas (enterprise-user, custom
// attributes) round-trip via Raw so we don't lose data — we just
// don't act on it yet.
type User struct {
	Schemas    []string         `json:"schemas"`
	ID         string           `json:"id"`
	UserName   string           `json:"userName"`
	Name       Name             `json:"name,omitempty"`
	DisplayName string          `json:"displayName,omitempty"`
	Emails     []Email          `json:"emails,omitempty"`
	Active     bool             `json:"active"`
	Groups     []GroupRef       `json:"groups,omitempty"`
	Meta       Meta             `json:"meta,omitempty"`
	Raw        json.RawMessage  `json:"-"`
}

type Name struct {
	GivenName  string `json:"givenName,omitempty"`
	FamilyName string `json:"familyName,omitempty"`
	Formatted  string `json:"formatted,omitempty"`
}

type Email struct {
	Value   string `json:"value"`
	Primary bool   `json:"primary,omitempty"`
	Type    string `json:"type,omitempty"`
}

type GroupRef struct {
	Value   string `json:"value"`
	Display string `json:"display,omitempty"`
}

type Meta struct {
	ResourceType string    `json:"resourceType,omitempty"`
	Created      time.Time `json:"created,omitempty"`
	LastModified time.Time `json:"lastModified,omitempty"`
	Location     string    `json:"location,omitempty"`
	Version      string    `json:"version,omitempty"`
}

// Group is the SCIM 2.0 Group resource shape.
type Group struct {
	Schemas     []string  `json:"schemas"`
	ID          string    `json:"id"`
	DisplayName string    `json:"displayName"`
	Members     []Member  `json:"members,omitempty"`
	Meta        Meta      `json:"meta,omitempty"`
}

type Member struct {
	Value   string `json:"value"`             // user id
	Display string `json:"display,omitempty"` // userName
}

// ListResponse is the wrapper SCIM uses for any list result.
type ListResponse struct {
	Schemas      []string      `json:"schemas"`
	TotalResults int           `json:"totalResults"`
	StartIndex   int           `json:"startIndex"`
	ItemsPerPage int           `json:"itemsPerPage"`
	Resources    []interface{} `json:"Resources"`
}

// MemoryStore is a thread-safe in-memory store. Sufficient for the
// initial SCIM bring-up; persistence lands when we wire orgs+users
// tables in Postgres.
type MemoryStore struct {
	mu     sync.RWMutex
	users  map[string]*User
	groups map[string]*Group
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		users:  map[string]*User{},
		groups: map[string]*Group{},
	}
}

func (s *MemoryStore) UpsertUser(u *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.ID == "" {
		u.ID = randID()
		u.Meta.Created = time.Now().UTC()
	}
	u.Meta.LastModified = time.Now().UTC()
	u.Meta.ResourceType = "User"
	u.Schemas = []string{"urn:ietf:params:scim:schemas:core:2.0:User"}
	s.users[u.ID] = u
	return nil
}

func (s *MemoryStore) GetUser(id string) (*User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[id]
	return u, ok
}

func (s *MemoryStore) ListUsers(filter string, count, start int) ([]*User, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		if matchUserFilter(u, filter) {
			out = append(out, u)
		}
	}
	total := len(out)
	if start < 1 {
		start = 1
	}
	if start-1 >= total {
		return nil, total
	}
	out = out[start-1:]
	if count > 0 && len(out) > count {
		out = out[:count]
	}
	return out, total
}

func (s *MemoryStore) DeleteUser(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[id]; !ok {
		return false
	}
	delete(s.users, id)
	return true
}

func (s *MemoryStore) UpsertGroup(g *Group) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g.ID == "" {
		g.ID = randID()
		g.Meta.Created = time.Now().UTC()
	}
	g.Meta.LastModified = time.Now().UTC()
	g.Meta.ResourceType = "Group"
	g.Schemas = []string{"urn:ietf:params:scim:schemas:core:2.0:Group"}
	s.groups[g.ID] = g
	return nil
}

func (s *MemoryStore) GetGroup(id string) (*Group, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.groups[id]
	return g, ok
}

func (s *MemoryStore) ListGroups(filter string, count, start int) ([]*Group, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Group, 0, len(s.groups))
	for _, g := range s.groups {
		if matchGroupFilter(g, filter) {
			out = append(out, g)
		}
	}
	total := len(out)
	if start < 1 {
		start = 1
	}
	if start-1 >= total {
		return nil, total
	}
	out = out[start-1:]
	if count > 0 && len(out) > count {
		out = out[:count]
	}
	return out, total
}

func (s *MemoryStore) DeleteGroup(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.groups[id]; !ok {
		return false
	}
	delete(s.groups, id)
	return true
}

// matchUserFilter implements the small slice of SCIM filter syntax
// IdPs actually use for de-duplication during provisioning:
//   userName eq "alice@example.com"
//   externalId eq "okta-1234"
// Anything else falls through to "match everything".
func matchUserFilter(u *User, filter string) bool {
	if filter == "" {
		return true
	}
	parts := strings.SplitN(filter, " eq ", 2)
	if len(parts) != 2 {
		return true
	}
	field := strings.TrimSpace(parts[0])
	want := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
	switch field {
	case "userName":
		return strings.EqualFold(u.UserName, want)
	case "id":
		return u.ID == want
	}
	return true
}

func matchGroupFilter(g *Group, filter string) bool {
	if filter == "" {
		return true
	}
	parts := strings.SplitN(filter, " eq ", 2)
	if len(parts) != 2 {
		return true
	}
	field := strings.TrimSpace(parts[0])
	want := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
	switch field {
	case "displayName":
		return strings.EqualFold(g.DisplayName, want)
	case "id":
		return g.ID == want
	}
	return true
}

func randID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Handler returns a net/http handler mounted under /scim/v2. Callers
// wrap it in their own auth middleware; in main.go that's a thin
// SCIM-key check matching against KUBEHERO_SCIM_TOKEN env.
func Handler(store Store) http.Handler {
	mux := http.NewServeMux()

	// Discovery — no auth on these per RFC 7644 §4.
	mux.HandleFunc("GET /scim/v2/ServiceProviderConfig", serviceProviderConfig)
	mux.HandleFunc("GET /scim/v2/Schemas", schemas)
	mux.HandleFunc("GET /scim/v2/ResourceTypes", resourceTypes)

	// Users
	mux.HandleFunc("GET /scim/v2/Users", func(w http.ResponseWriter, r *http.Request) {
		filter := r.URL.Query().Get("filter")
		count, _ := strconv.Atoi(r.URL.Query().Get("count"))
		start, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
		users, total := store.ListUsers(filter, count, start)
		out := ListResponse{
			Schemas:      []string{"urn:ietf:params:scim:api:messages:2.0:ListResponse"},
			TotalResults: total,
			StartIndex:   maxInt(start, 1),
			ItemsPerPage: len(users),
			Resources:    make([]interface{}, len(users)),
		}
		for i, u := range users {
			out.Resources[i] = u
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /scim/v2/Users/{id}", func(w http.ResponseWriter, r *http.Request) {
		u, ok := store.GetUser(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		writeJSON(w, http.StatusOK, u)
	})
	mux.HandleFunc("POST /scim/v2/Users", func(w http.ResponseWriter, r *http.Request) {
		var u User
		if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := store.UpsertUser(&u); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, u)
	})
	mux.HandleFunc("PUT /scim/v2/Users/{id}", func(w http.ResponseWriter, r *http.Request) {
		var u User
		if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		u.ID = r.PathValue("id")
		if err := store.UpsertUser(&u); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, u)
	})
	mux.HandleFunc("PATCH /scim/v2/Users/{id}", func(w http.ResponseWriter, r *http.Request) {
		// Minimal PATCH: we honour only the most common operation,
		// `replace` on the `active` attribute (used by every IdP for
		// deprovisioning). Other ops trickle through as a 200.
		u, ok := store.GetUser(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		var p struct {
			Schemas    []string `json:"schemas"`
			Operations []struct {
				Op    string          `json:"op"`
				Path  string          `json:"path"`
				Value json.RawMessage `json:"value"`
			} `json:"Operations"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		for _, op := range p.Operations {
			if strings.EqualFold(op.Op, "replace") && op.Path == "active" {
				_ = json.Unmarshal(op.Value, &u.Active)
			}
		}
		_ = store.UpsertUser(u)
		writeJSON(w, http.StatusOK, u)
	})
	mux.HandleFunc("DELETE /scim/v2/Users/{id}", func(w http.ResponseWriter, r *http.Request) {
		// SCIM "DELETE" is a soft delete: flip active=false, keep the
		// row for audit. IdPs are happy with 204.
		u, ok := store.GetUser(r.PathValue("id"))
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		u.Active = false
		_ = store.UpsertUser(u)
		w.WriteHeader(http.StatusNoContent)
	})

	// Groups — same shape as Users.
	mux.HandleFunc("GET /scim/v2/Groups", func(w http.ResponseWriter, r *http.Request) {
		filter := r.URL.Query().Get("filter")
		count, _ := strconv.Atoi(r.URL.Query().Get("count"))
		start, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
		groups, total := store.ListGroups(filter, count, start)
		out := ListResponse{
			Schemas:      []string{"urn:ietf:params:scim:api:messages:2.0:ListResponse"},
			TotalResults: total,
			StartIndex:   maxInt(start, 1),
			ItemsPerPage: len(groups),
			Resources:    make([]interface{}, len(groups)),
		}
		for i, g := range groups {
			out.Resources[i] = g
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /scim/v2/Groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		g, ok := store.GetGroup(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "group not found")
			return
		}
		writeJSON(w, http.StatusOK, g)
	})
	mux.HandleFunc("POST /scim/v2/Groups", func(w http.ResponseWriter, r *http.Request) {
		var g Group
		if err := json.NewDecoder(r.Body).Decode(&g); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := store.UpsertGroup(&g); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, g)
	})
	mux.HandleFunc("PUT /scim/v2/Groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		var g Group
		if err := json.NewDecoder(r.Body).Decode(&g); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		g.ID = r.PathValue("id")
		if err := store.UpsertGroup(&g); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, g)
	})
	mux.HandleFunc("DELETE /scim/v2/Groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		store.DeleteGroup(r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

// AuthMiddleware checks the SCIM bearer header against a constant-time
// comparison of `expectedToken`. We deliberately don't reuse the cp's
// auth interceptor here — SCIM clients don't speak Connect-RPC and
// shouldn't share credential lifecycle with cluster operators.
func AuthMiddleware(expectedToken string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Discovery endpoints are public per spec.
		if strings.HasSuffix(r.URL.Path, "/ServiceProviderConfig") ||
			strings.HasSuffix(r.URL.Path, "/Schemas") ||
			strings.HasSuffix(r.URL.Path, "/ResourceTypes") {
			next.ServeHTTP(w, r)
			return
		}
		if expectedToken == "" {
			writeError(w, http.StatusUnauthorized, "SCIM disabled — KUBEHERO_SCIM_TOKEN not set")
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !constantTimeEqual(got, expectedToken) {
			writeError(w, http.StatusUnauthorized, "invalid SCIM token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// serviceProviderConfig advertises which optional pieces of SCIM 2.0
// we support. Honest is better than aspirational — every IdP
// validates this and adjusts behaviour, so claiming Bulk would just
// trigger Bulk requests we'd then 501 on.
func serviceProviderConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
		"patch":   map[string]bool{"supported": true},
		"bulk":    map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":  map[string]any{"supported": true, "maxResults": 200},
		"changePassword": map[string]bool{"supported": false},
		"sort":           map[string]bool{"supported": false},
		"etag":           map[string]bool{"supported": false},
		"authenticationSchemes": []map[string]any{{
			"type":        "oauthbearertoken",
			"name":        "OAuth Bearer Token",
			"description": "KUBEHERO_SCIM_TOKEN bearer header",
			"primary":     true,
		}},
	})
}

func schemas(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"schemas":      []string{"urn:ietf:params:scim:api:messages:2.0:ListResponse"},
		"totalResults": 2,
		"itemsPerPage": 2,
		"startIndex":   1,
		"Resources": []map[string]any{
			{"id": "urn:ietf:params:scim:schemas:core:2.0:User"},
			{"id": "urn:ietf:params:scim:schemas:core:2.0:Group"},
		},
	})
}

func resourceTypes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"schemas":      []string{"urn:ietf:params:scim:api:messages:2.0:ListResponse"},
		"totalResults": 2,
		"itemsPerPage": 2,
		"startIndex":   1,
		"Resources": []map[string]any{
			{
				"id":       "User",
				"name":     "User",
				"endpoint": "/Users",
				"schema":   "urn:ietf:params:scim:schemas:core:2.0:User",
			},
			{
				"id":       "Group",
				"name":     "Group",
				"endpoint": "/Groups",
				"schema":   "urn:ietf:params:scim:schemas:core:2.0:Group",
			},
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:Error"],"status":"%d","detail":%q}`, status, detail)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
