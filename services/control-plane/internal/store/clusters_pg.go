// SPDX-License-Identifier: BUSL-1.1
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type ClustersPG struct{ DB *sql.DB }

func (s *ClustersPG) Register(ctx context.Context, c *Cluster) error {
	// OrgID may be passed in two shapes:
	//   - a real UUID (already resolved upstream)
	//   - an org slug like "default" (the typical RPC caller path)
	// Look up by slug first; if it resolves, use the matching UUID.
	// If the lookup misses, we fall through with the original value
	// and let Postgres surface the type error.
	orgID := c.OrgID
	const lookup = `SELECT id::text FROM orgs WHERE slug = $1`
	if !looksLikeUUID(orgID) {
		var resolved string
		if err := s.DB.QueryRowContext(ctx, lookup, orgID).Scan(&resolved); err == nil {
			orgID = resolved
		}
	}

	const q = `
	  INSERT INTO clusters (org_id, slug, name, cloud, region, cert_fingerprint, state)
	  VALUES ($1, $2, $3, $4, $5, $6, COALESCE(NULLIF($7,''),'healthy'))
	  ON CONFLICT (org_id, slug) DO UPDATE SET
	    name             = EXCLUDED.name,
	    cloud            = EXCLUDED.cloud,
	    region           = EXCLUDED.region,
	    cert_fingerprint = EXCLUDED.cert_fingerprint
	  RETURNING id, created_at`
	return s.DB.QueryRowContext(ctx, q,
		orgID, c.Slug, c.Name, c.Cloud, c.Region, c.CertFingerprint, c.State,
	).Scan(&c.ID, &c.CreatedAt)
}

// looksLikeUUID returns true for the canonical 36-char hyphenated form.
// We don't validate hex precisely; the goal is to short-circuit slug
// lookups when the caller has already resolved an org UUID.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

func (s *ClustersPG) Get(ctx context.Context, id string) (*Cluster, error) {
	const q = `
	  SELECT id::text, org_id::text, slug, name, cloud, region,
	         COALESCE(cert_fingerprint,''), nodes_count, last_seen, state, created_at
	  FROM clusters WHERE id = $1`
	c := &Cluster{}
	err := s.DB.QueryRowContext(ctx, q, id).Scan(
		&c.ID, &c.OrgID, &c.Slug, &c.Name, &c.Cloud, &c.Region,
		&c.CertFingerprint, &c.NodesCount, &c.LastSeen, &c.State, &c.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("cluster %s not found", id)
	}
	return c, err
}

func (s *ClustersPG) List(ctx context.Context, orgID string, pageSize, offset int) ([]*Cluster, error) {
	if pageSize <= 0 || pageSize > 500 {
		pageSize = 100
	}
	const q = `
	  SELECT id::text, org_id::text, slug, name, cloud, region,
	         COALESCE(cert_fingerprint,''), nodes_count, last_seen, state, created_at
	  FROM clusters WHERE org_id = $1
	  ORDER BY created_at DESC LIMIT $2 OFFSET $3`
	rows, err := s.DB.QueryContext(ctx, q, orgID, pageSize, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Cluster
	for rows.Next() {
		c := &Cluster{}
		if err := rows.Scan(&c.ID, &c.OrgID, &c.Slug, &c.Name, &c.Cloud, &c.Region,
			&c.CertFingerprint, &c.NodesCount, &c.LastSeen, &c.State, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *ClustersPG) Touch(ctx context.Context, id string, nodes int32, state string) error {
	const q = `
	  UPDATE clusters SET nodes_count = $2, state = COALESCE(NULLIF($3,''), state),
	                      last_seen = $4
	  WHERE id = $1`
	_, err := s.DB.ExecContext(ctx, q, id, nodes, state, time.Now().UTC())
	return err
}
