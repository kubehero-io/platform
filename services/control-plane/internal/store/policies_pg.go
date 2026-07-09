// SPDX-License-Identifier: BUSL-1.1
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type PoliciesPG struct{ DB *sql.DB }

func (s *PoliciesPG) Upsert(ctx context.Context, p *Policy) error {
	const q = `
	  INSERT INTO policies (cluster_id, kind, namespace, name, spec, generation)
	  VALUES ($1, $2, $3, $4, $5, $6)
	  ON CONFLICT (cluster_id, kind, namespace, name) DO UPDATE SET
	    spec       = EXCLUDED.spec,
	    generation = EXCLUDED.generation
	  RETURNING id, updated_at`
	return s.DB.QueryRowContext(ctx, q,
		p.ClusterID, p.Kind, p.Namespace, p.Name, p.SpecJSON, p.Generation,
	).Scan(&p.ID, &p.UpdatedAt)
}

func (s *PoliciesPG) Get(ctx context.Context, id string) (*Policy, error) {
	const q = `
	  SELECT id::text, cluster_id::text, kind, namespace, name, spec,
	         armed, armed_by::text, armed_at, generation, last_eval,
	         COALESCE(last_eval_result,''), updated_at
	  FROM policies WHERE id = $1`
	p := &Policy{}
	var armedBy sql.NullString
	err := s.DB.QueryRowContext(ctx, q, id).Scan(
		&p.ID, &p.ClusterID, &p.Kind, &p.Namespace, &p.Name, &p.SpecJSON,
		&p.Armed, &armedBy, &p.ArmedAt, &p.Generation, &p.LastEval,
		&p.LastEvalResult, &p.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("policy %s not found", id)
	}
	if armedBy.Valid {
		p.ArmedByUserID = &armedBy.String
	}
	return p, err
}

func (s *PoliciesPG) List(ctx context.Context, clusterID string) ([]*Policy, error) {
	const q = `
	  SELECT id::text, cluster_id::text, kind, namespace, name, spec,
	         armed, armed_by::text, armed_at, generation, last_eval,
	         COALESCE(last_eval_result,''), updated_at
	  FROM policies WHERE cluster_id = $1 ORDER BY updated_at DESC`
	rows, err := s.DB.QueryContext(ctx, q, clusterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Policy
	for rows.Next() {
		p := &Policy{}
		var armedBy sql.NullString
		if err := rows.Scan(&p.ID, &p.ClusterID, &p.Kind, &p.Namespace, &p.Name,
			&p.SpecJSON, &p.Armed, &armedBy, &p.ArmedAt, &p.Generation, &p.LastEval,
			&p.LastEvalResult, &p.UpdatedAt); err != nil {
			return nil, err
		}
		if armedBy.Valid {
			p.ArmedByUserID = &armedBy.String
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *PoliciesPG) Arm(ctx context.Context, id, userID string) error {
	const q = `UPDATE policies SET armed = TRUE, armed_by = $2, armed_at = $3 WHERE id = $1`
	_, err := s.DB.ExecContext(ctx, q, id, userID, time.Now().UTC())
	return err
}

func (s *PoliciesPG) Disarm(ctx context.Context, id string) error {
	const q = `UPDATE policies SET armed = FALSE, armed_by = NULL, armed_at = NULL WHERE id = $1`
	_, err := s.DB.ExecContext(ctx, q, id)
	return err
}

// SetArmedByName flips the kill-switch on a policy addressed by
// (cluster, name) rather than the surrogate id Arm/Disarm use.
// clusterID may be a UUID or a cluster slug — slugs are resolved
// against the clusters table, mirroring AuditPG's tolerance. armed_by
// is a UUID FK to users(id), so the actor is only stamped there when
// it is UUID-shaped; CLI / API-key actors are recorded in the audit
// log instead.
func (s *PoliciesPG) SetArmedByName(ctx context.Context, clusterID, name string, armed bool, actor string) (*Policy, error) {
	if !looksLikeUUID(clusterID) {
		var resolved string
		if err := s.DB.QueryRowContext(ctx,
			`SELECT id::text FROM clusters WHERE slug = $1 LIMIT 1`, clusterID).Scan(&resolved); err != nil {
			if err == sql.ErrNoRows {
				return nil, fmt.Errorf("cluster %q: %w", clusterID, ErrNotFound)
			}
			return nil, err
		}
		clusterID = resolved
	}

	var armedBy sql.NullString
	if armed && looksLikeUUID(actor) {
		armedBy = sql.NullString{String: actor, Valid: true}
	}
	var armedAt sql.NullTime
	if armed {
		armedAt = sql.NullTime{Time: time.Now().UTC(), Valid: true}
	}

	const q = `
	  UPDATE policies SET armed = $3, armed_by = $4, armed_at = $5
	  WHERE cluster_id = $1 AND name = $2
	  RETURNING id::text, cluster_id::text, kind, namespace, name, spec,
	            armed, armed_by::text, armed_at, generation, last_eval,
	            COALESCE(last_eval_result,''), updated_at`
	p := &Policy{}
	var by sql.NullString
	err := s.DB.QueryRowContext(ctx, q, clusterID, name, armed, armedBy, armedAt).Scan(
		&p.ID, &p.ClusterID, &p.Kind, &p.Namespace, &p.Name, &p.SpecJSON,
		&p.Armed, &by, &p.ArmedAt, &p.Generation, &p.LastEval,
		&p.LastEvalResult, &p.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("policy %q in cluster %s: %w", name, clusterID, ErrNotFound)
	}
	if by.Valid {
		p.ArmedByUserID = &by.String
	}
	return p, err
}

func (s *PoliciesPG) RecordEval(ctx context.Context, id, result string) error {
	const q = `UPDATE policies SET last_eval = $2, last_eval_result = $3 WHERE id = $1`
	_, err := s.DB.ExecContext(ctx, q, id, time.Now().UTC(), result)
	return err
}
