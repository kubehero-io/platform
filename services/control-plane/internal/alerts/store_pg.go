// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package alerts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// PGStore is the Postgres Store (migration 0003_alerts).
type PGStore struct{ DB *sql.DB }

var _ Store = (*PGStore)(nil)

func (s *PGStore) Persistent() bool { return true }

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const ruleCols = `id::text, name, description, kind, query, op, threshold, pending_for_ms, severity,
	channels, labels, annotations, enabled, eval_interval_ms, created_by, created_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanRule(sc scanner) (*Rule, error) {
	var (
		r                   Rule
		pendingMS, evalMS   int64
		chans, lbls, annots []byte
	)
	if err := sc.Scan(&r.ID, &r.Name, &r.Description, &r.Kind, &r.Query, &r.Op, &r.Threshold, &pendingMS,
		&r.Severity, &chans, &lbls, &annots, &r.Enabled, &evalMS, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.PendingFor = time.Duration(pendingMS) * time.Millisecond
	r.EvalInterval = time.Duration(evalMS) * time.Millisecond
	if err := json.Unmarshal(chans, &r.Channels); err != nil {
		return nil, fmt.Errorf("rule %s channels: %w", r.ID, err)
	}
	if err := json.Unmarshal(lbls, &r.Labels); err != nil {
		return nil, fmt.Errorf("rule %s labels: %w", r.ID, err)
	}
	if err := json.Unmarshal(annots, &r.Annotations); err != nil {
		return nil, fmt.Errorf("rule %s annotations: %w", r.ID, err)
	}
	return &r, nil
}

func (s *PGStore) ListRules(ctx context.Context) ([]*Rule, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+ruleCols+` FROM alert_rules ORDER BY name LIMIT $1`, maxRules)
	if err != nil {
		return nil, fmt.Errorf("list alert rules: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []*Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PGStore) GetRule(ctx context.Context, id string) (*Rule, error) {
	if !uuidRE.MatchString(id) {
		return nil, ErrNotFound
	}
	r, err := scanRule(s.DB.QueryRowContext(ctx, `SELECT `+ruleCols+` FROM alert_rules WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

func jsonOf(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func (s *PGStore) UpsertRule(ctx context.Context, r *Rule) (*Rule, error) {
	chans := r.Channels
	if chans == nil {
		chans = []string{}
	}
	lbls, annots := r.Labels, r.Annotations
	if lbls == nil {
		lbls = map[string]string{}
	}
	if annots == nil {
		annots = map[string]string{}
	}
	args := []any{r.Name, r.Description, r.Kind, r.Query, r.Op, r.Threshold, r.PendingFor.Milliseconds(), r.Severity,
		jsonOf(chans), jsonOf(lbls), jsonOf(annots), r.Enabled, r.EvalInterval.Milliseconds()}
	var row *sql.Row
	if r.ID == "" {
		var n int
		if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM alert_rules`).Scan(&n); err != nil {
			return nil, err
		}
		if n >= maxRules {
			return nil, ErrTooMany
		}
		row = s.DB.QueryRowContext(ctx, `
			INSERT INTO alert_rules (name, description, kind, query, op, threshold, pending_for_ms, severity,
			                         channels, labels, annotations, enabled, eval_interval_ms, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			RETURNING `+ruleCols, append(args, r.CreatedBy)...)
	} else {
		if !uuidRE.MatchString(r.ID) {
			return nil, ErrNotFound
		}
		row = s.DB.QueryRowContext(ctx, `
			UPDATE alert_rules SET name=$1, description=$2, kind=$3, query=$4, op=$5, threshold=$6,
			       pending_for_ms=$7, severity=$8, channels=$9, labels=$10, annotations=$11, enabled=$12,
			       eval_interval_ms=$13
			WHERE id = $14
			RETURNING `+ruleCols, append(args, r.ID)...)
	}
	out, err := scanRule(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case isUniqueViolation(err):
		return nil, ErrNameTaken
	case err != nil:
		return nil, fmt.Errorf("upsert alert rule: %w", err)
	}
	return out, nil
}

func (s *PGStore) DeleteRule(ctx context.Context, id string) error {
	if !uuidRE.MatchString(id) {
		return ErrNotFound
	}
	res, err := s.DB.ExecContext(ctx, `DELETE FROM alert_rules WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete alert rule: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const alertCols = `id, rule_id::text, fingerprint, labels, state, value, summary, description, link_path,
	started_at, fired_at, resolved_at, last_eval_at, last_notified_at`

func scanAlert(sc scanner) (*Alert, error) {
	var (
		a                         Alert
		lbls                      []byte
		fired, resolved, notified sql.NullTime
	)
	if err := sc.Scan(&a.ID, &a.RuleID, &a.Fingerprint, &lbls, &a.State, &a.Value, &a.Summary, &a.Description,
		&a.LinkPath, &a.StartedAt, &fired, &resolved, &a.LastEvalAt, &notified); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(lbls, &a.Labels); err != nil {
		return nil, fmt.Errorf("alert %s labels: %w", a.ID, err)
	}
	a.FiredAt, a.ResolvedAt, a.LastNotifiedAt = fired.Time, resolved.Time, notified.Time
	a.StartedAt, a.LastEvalAt = a.StartedAt.UTC(), a.LastEvalAt.UTC()
	return &a, nil
}

func (s *PGStore) queryAlerts(ctx context.Context, where string, args ...any) ([]*Alert, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+alertCols+` FROM alert_states `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []*Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *PGStore) RuleAlerts(ctx context.Context, ruleID string) ([]*Alert, error) {
	return s.queryAlerts(ctx, `WHERE rule_id = $1`, ruleID)
}

func (s *PGStore) ListAlerts(ctx context.Context) ([]*Alert, error) {
	return s.queryAlerts(ctx, `ORDER BY last_eval_at DESC LIMIT 50000`)
}

func nullTime(t time.Time) sql.NullTime { return sql.NullTime{Time: t, Valid: !t.IsZero()} }

func (s *PGStore) SaveAlerts(ctx context.Context, ruleID string, upserts []*Alert, deleted []string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if len(deleted) > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM alert_states WHERE rule_id = $1 AND id = ANY($2)`, ruleID, deleted); err != nil {
			return fmt.Errorf("delete alerts: %w", err)
		}
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO alert_states (id, rule_id, fingerprint, labels, state, value, summary, description, link_path,
		                          started_at, fired_at, resolved_at, last_eval_at, last_notified_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (id) DO UPDATE SET labels=EXCLUDED.labels, state=EXCLUDED.state, value=EXCLUDED.value,
		       summary=EXCLUDED.summary, description=EXCLUDED.description, link_path=EXCLUDED.link_path,
		       started_at=EXCLUDED.started_at, fired_at=EXCLUDED.fired_at, resolved_at=EXCLUDED.resolved_at,
		       last_eval_at=EXCLUDED.last_eval_at, last_notified_at=EXCLUDED.last_notified_at`)
	if err != nil {
		return err
	}
	defer stmt.Close() //nolint:errcheck
	for _, a := range upserts {
		if _, err := stmt.ExecContext(ctx, a.ID, ruleID, a.Fingerprint, jsonOf(a.Labels), a.State, a.Value, a.Summary,
			a.Description, a.LinkPath, a.StartedAt, nullTime(a.FiredAt), nullTime(a.ResolvedAt), a.LastEvalAt,
			nullTime(a.LastNotifiedAt)); err != nil {
			if isForeignKeyViolation(err) {
				return nil // rule deleted mid-evaluation
			}
			return fmt.Errorf("save alert %s: %w", a.ID, err)
		}
	}
	return tx.Commit()
}

func isForeignKeyViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23503"
}

func (s *PGStore) CreateSilence(ctx context.Context, sl *Silence) (*Silence, error) {
	out := *sl
	out.Matchers = cloneMap(sl.Matchers)
	err := s.DB.QueryRowContext(ctx, `
		INSERT INTO alert_silences (matchers, starts_at, ends_at, created_by, comment)
		VALUES ($1,$2,$3,$4,$5) RETURNING id::text, created_at`,
		jsonOf(sl.Matchers), sl.StartsAt, sl.EndsAt, sl.CreatedBy, sl.Comment).Scan(&out.ID, &out.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create silence: %w", err)
	}
	return &out, nil
}

func (s *PGStore) ListSilences(ctx context.Context, includeExpired bool, now time.Time) ([]*Silence, error) {
	q := `SELECT id::text, matchers, starts_at, ends_at, created_by, comment, created_at FROM alert_silences`
	args := []any{}
	if !includeExpired {
		q += ` WHERE ends_at > $1`
		args = append(args, now)
	}
	q += ` ORDER BY ends_at DESC LIMIT 10000`
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list silences: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []*Silence
	for rows.Next() {
		var (
			sl Silence
			m  []byte
		)
		if err := rows.Scan(&sl.ID, &m, &sl.StartsAt, &sl.EndsAt, &sl.CreatedBy, &sl.Comment, &sl.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(m, &sl.Matchers); err != nil {
			return nil, err
		}
		out = append(out, &sl)
	}
	return out, rows.Err()
}

func (s *PGStore) ExpireSilence(ctx context.Context, id string, now time.Time) error {
	if !uuidRE.MatchString(id) {
		return ErrNotFound
	}
	res, err := s.DB.ExecContext(ctx, `
		UPDATE alert_silences
		SET ends_at = CASE WHEN ends_at > $2 THEN GREATEST($2, starts_at + interval '1 millisecond') ELSE ends_at END
		WHERE id = $1`, id, now)
	if err != nil {
		return fmt.Errorf("expire silence: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PGStore) RecordNotifications(ctx context.Context, ns []Notification) error {
	if len(ns) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO alert_notifications (at, alert_id, rule_id, event, channel, ok, error)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`)
	if err != nil {
		return err
	}
	defer stmt.Close() //nolint:errcheck
	for _, n := range ns {
		var rule any
		if uuidRE.MatchString(n.RuleID) {
			rule = n.RuleID
		}
		if _, err := stmt.ExecContext(ctx, n.At, n.AlertID, rule, n.Event, n.Channel, n.OK, truncate(n.Error, 1000)); err != nil {
			return fmt.Errorf("record notification: %w", err)
		}
	}
	return tx.Commit()
}

// PruneNotifications drops log rows older than keep.
func (s *PGStore) PruneNotifications(ctx context.Context, keep time.Duration) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM alert_notifications WHERE at < $1`, time.Now().Add(-keep))
	return err
}

func (s *PGStore) SeedDefaults(ctx context.Context, rules []*Rule) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.ExecContext(ctx, `INSERT INTO alert_meta (key, value) VALUES ('default_rules_seeded', $1)
		ON CONFLICT (key) DO NOTHING`, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, fmt.Errorf("seed marker: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil // seeded (or deliberately emptied) before
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM alert_rules`).Scan(&existing); err != nil {
		return false, err
	}
	if existing > 0 {
		return false, tx.Commit()
	}
	for _, r := range rules {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO alert_rules (name, description, kind, query, op, threshold, pending_for_ms, severity,
			                         channels, labels, annotations, enabled, eval_interval_ms, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			r.Name, r.Description, r.Kind, r.Query, r.Op, r.Threshold, r.PendingFor.Milliseconds(), r.Severity,
			jsonOf(nonNilSlice(r.Channels)), jsonOf(nonNilMap(r.Labels)), jsonOf(nonNilMap(r.Annotations)),
			r.Enabled, r.EvalInterval.Milliseconds(), r.CreatedBy); err != nil {
			return false, fmt.Errorf("seed rule %q: %w", r.Name, err)
		}
	}
	return true, tx.Commit()
}

func nonNilSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ─── leader election ─────────────────────────────────────────────────

// leaderLockKey is the advisory lock id ("kubehero" as bytes).
const leaderLockKey int64 = 0x6b75626568657230

// PGLeader elects one evaluating replica with a session-level advisory
// lock held on a dedicated connection: if that connection drops, the
// lock is released and another replica takes over.
type PGLeader struct {
	DB   *sql.DB
	mu   sync.Mutex
	conn *sql.Conn
}

// IsLeader (re)acquires or verifies leadership.
func (l *PGLeader) IsLeader(ctx context.Context) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if l.conn != nil {
		if err := l.conn.PingContext(ctx); err == nil {
			return true
		}
		_ = l.conn.Close()
		l.conn = nil
	}
	conn, err := l.DB.Conn(ctx)
	if err != nil {
		return false
	}
	var ok bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, leaderLockKey).Scan(&ok); err != nil || !ok {
		_ = conn.Close()
		return false
	}
	l.conn = conn
	return true
}

// Release gives up leadership (shutdown).
func (l *PGLeader) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = l.conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, leaderLockKey)
		_ = l.conn.Close()
		l.conn = nil
	}
}
