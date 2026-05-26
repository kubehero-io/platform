// SPDX-License-Identifier: BUSL-1.1
package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
)

// AuditPG is the append-only audit log. Each row is HMAC-signed so
// downstream SIEMs can verify an exported event wasn't tampered with.
type AuditPG struct {
	DB     *sql.DB
	Secret []byte // HMAC key — rotate via SETTINGS; never logged
}

func (s *AuditPG) Append(ctx context.Context, e *AuditEntry) (int64, error) {
	if e.Outcome == "" {
		e.Outcome = "success"
	}

	// Resolve slug → UUID for org_id and cluster_id when callers pass
	// the friendlier slug (e.g. "default", "eks-use1-prod"). Mirrors
	// ClustersPG.Register's behaviour so the AppendAuditEntry RPC
	// can stay slug-based.
	orgID := s.resolveOrgID(ctx, e.OrgID)
	clusterID := s.resolveClusterID(ctx, e.ClusterID)

	sig := s.sign(e)
	const q = `
	  INSERT INTO audit_log (org_id, cluster_id, actor_sub, actor_email, action,
	                         target_kind, target_name, payload, previous_spec,
	                         outcome, signature, request_id)
	  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	  RETURNING id`
	var id int64
	err := s.DB.QueryRowContext(ctx, q,
		orgID, clusterID, e.ActorSub, e.ActorEmail, e.Action,
		e.TargetKind, e.TargetName, e.Payload, e.PreviousSpec,
		e.Outcome, sig, e.RequestID,
	).Scan(&id)
	return id, err
}

// resolveOrgID resolves a slug pointer to a UUID-pointer; pass-through
// when the input is already UUID-shaped or nil.
func (s *AuditPG) resolveOrgID(ctx context.Context, in *string) *string {
	if in == nil || *in == "" || looksLikeUUID(*in) {
		return in
	}
	var id string
	if err := s.DB.QueryRowContext(ctx, `SELECT id::text FROM orgs WHERE slug = $1`, *in).Scan(&id); err != nil {
		// Unknown slug — leave as nil so the FK column stays unset
		// rather than producing a UUID syntax error.
		return nil
	}
	return &id
}

// resolveClusterID treats the input as either a UUID or a cluster slug
// scoped to the matching org. We don't carry the org slug here, so the
// lookup matches on slug alone — fine while we run a single org.
func (s *AuditPG) resolveClusterID(ctx context.Context, in *string) *string {
	if in == nil || *in == "" || looksLikeUUID(*in) {
		return in
	}
	var id string
	if err := s.DB.QueryRowContext(ctx, `SELECT id::text FROM clusters WHERE slug = $1 LIMIT 1`, *in).Scan(&id); err != nil {
		return nil
	}
	return &id
}

func (s *AuditPG) List(ctx context.Context, orgID string, limit int) ([]*AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// Mirror Append's slug-tolerance — accept either a UUID or a slug
	// like "default" so the RPC layer can stay slug-based.
	if !looksLikeUUID(orgID) {
		var resolved string
		if err := s.DB.QueryRowContext(ctx,
			`SELECT id::text FROM orgs WHERE slug = $1`, orgID).Scan(&resolved); err == nil {
			orgID = resolved
		}
	}
	const q = `
	  SELECT at, org_id::text, cluster_id::text, actor_sub, actor_email, action,
	         target_kind, target_name, payload, previous_spec, outcome, request_id
	  FROM audit_log WHERE org_id = $1 ORDER BY at DESC LIMIT $2`
	rows, err := s.DB.QueryContext(ctx, q, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		e := &AuditEntry{}
		var orgID, clusterID sql.NullString
		if err := rows.Scan(&e.At, &orgID, &clusterID, &e.ActorSub, &e.ActorEmail,
			&e.Action, &e.TargetKind, &e.TargetName, &e.Payload, &e.PreviousSpec,
			&e.Outcome, &e.RequestID); err != nil {
			return nil, err
		}
		if orgID.Valid {
			e.OrgID = &orgID.String
		}
		if clusterID.Valid {
			e.ClusterID = &clusterID.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// sign emits a deterministic HMAC-SHA256 over the identifying fields so
// a downstream SIEM can verify an exported event wasn't tampered with.
func (s *AuditPG) sign(e *AuditEntry) string {
	if len(s.Secret) == 0 {
		return "" // signing disabled — dev mode
	}
	h := hmac.New(sha256.New, s.Secret)
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|", e.At.UTC().Format("2006-01-02T15:04:05.000Z"),
		strOr(e.OrgID), strOr(e.ClusterID), e.ActorSub, e.Action)
	h.Write(e.Payload)
	return "hmac-sha256:" + hex.EncodeToString(h.Sum(nil))
}

func strOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
