// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package store owns the typed data model on top of Postgres + ClickHouse.
// Callers (RPC handlers, policy engine, audit forwarders) depend on the
// interfaces here so they can be tested against an in-memory fake.

package store

import (
	"context"
	"time"
)

type Cluster struct {
	ID              string
	OrgID           string
	Slug            string
	Name            string
	Cloud           string
	Region          string
	CertFingerprint string
	NodesCount      int32
	LastSeen        *time.Time
	State           string
	CreatedAt       time.Time
}

type Policy struct {
	ID             string
	ClusterID      string
	Kind           string // BudgetPolicy · CeilingPolicy · RightsizingPolicy
	Namespace      string
	Name           string
	SpecJSON       []byte
	Armed          bool
	ArmedByUserID  *string
	ArmedAt        *time.Time
	Generation     int64
	LastEval       *time.Time
	LastEvalResult string
	UpdatedAt      time.Time
}

type AuditEntry struct {
	At           time.Time
	OrgID        *string
	ClusterID    *string
	ActorSub     string
	ActorEmail   string
	Action       string
	TargetKind   string
	TargetName   string
	Payload      []byte // JSON
	PreviousSpec []byte // JSON — enables kubehero undo
	Outcome      string
	RequestID    string
}

// ClusterStore — cluster CRUD + heartbeat.
type ClusterStore interface {
	Register(ctx context.Context, c *Cluster) error
	Get(ctx context.Context, id string) (*Cluster, error)
	List(ctx context.Context, orgID string, pageSize, offset int) ([]*Cluster, error)
	Touch(ctx context.Context, id string, nodes int32, state string) error
}

// PolicyStore — mirror of CRDs observed from each cluster's operator.
type PolicyStore interface {
	Upsert(ctx context.Context, p *Policy) error
	Get(ctx context.Context, id string) (*Policy, error)
	List(ctx context.Context, clusterID string) ([]*Policy, error)
	Arm(ctx context.Context, id, userID string) error
	Disarm(ctx context.Context, id string) error
	RecordEval(ctx context.Context, id, result string) error
}

// AuditStore — append-only + read-back for the dashboard ceiling log.
type AuditStore interface {
	Append(ctx context.Context, e *AuditEntry) (id int64, err error)
	List(ctx context.Context, orgID string, limit int) ([]*AuditEntry, error)
}
