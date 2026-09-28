// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package store

import (
	"context"
	"database/sql"
	"fmt"
)

// PolicyRow is a policy together with its cluster's registered names,
// for fleet-wide listings.
type PolicyRow struct {
	Policy
	ClusterSlug string
	ClusterName string
}

// PolicyLister lists policies across the fleet (ListPolicies).
type PolicyLister interface {
	ListAll(ctx context.Context, cluster string) ([]*PolicyRow, error)
}

var _ PolicyLister = (*PoliciesPG)(nil)

// maxPolicyRows bounds a fleet-wide listing.
const maxPolicyRows = 2000

// ListAll returns every policy (or only those of one cluster, addressed
// by UUID or slug), ordered by kind then name.
func (s *PoliciesPG) ListAll(ctx context.Context, cluster string) ([]*PolicyRow, error) {
	const q = `
	  SELECT p.id::text, p.cluster_id::text, p.kind, p.namespace, p.name, p.spec,
	         p.armed, p.armed_at, p.generation, p.last_eval,
	         COALESCE(p.last_eval_result,''), p.updated_at, c.slug, c.name
	  FROM policies p JOIN clusters c ON c.id = p.cluster_id
	  WHERE $1 = '' OR p.cluster_id::text = $1 OR c.slug = $1
	  ORDER BY p.kind, p.name
	  LIMIT $2`
	rows, err := s.DB.QueryContext(ctx, q, cluster, maxPolicyRows)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []*PolicyRow
	for rows.Next() {
		r := &PolicyRow{}
		var armedAt, lastEval sql.NullTime
		if err := rows.Scan(&r.ID, &r.ClusterID, &r.Kind, &r.Namespace, &r.Name, &r.SpecJSON,
			&r.Armed, &armedAt, &r.Generation, &lastEval, &r.LastEvalResult, &r.UpdatedAt,
			&r.ClusterSlug, &r.ClusterName); err != nil {
			return nil, fmt.Errorf("scan policy: %w", err)
		}
		if armedAt.Valid {
			t := armedAt.Time
			r.ArmedAt = &t
		}
		if lastEval.Valid {
			t := lastEval.Time
			r.LastEval = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
