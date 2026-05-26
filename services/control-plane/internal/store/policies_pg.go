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

func (s *PoliciesPG) RecordEval(ctx context.Context, id, result string) error {
	const q = `UPDATE policies SET last_eval = $2, last_eval_result = $3 WHERE id = $1`
	_, err := s.DB.ExecContext(ctx, q, id, time.Now().UTC(), result)
	return err
}
