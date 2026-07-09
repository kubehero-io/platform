// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/store"
)

// Alerter is the narrow notification surface ArmPolicy needs. Both
// *alerter.Router and the engine's Alerter satisfy it; tests swap in
// a recording fake.
type Alerter interface {
	Alert(ctx context.Context, channels []string, message string) error
}

// ArmPolicy flips a policy's kill-switch. The armed bit is persisted
// via the policy store, an HMAC-signed audit row records who/why, and
// the configured alert channels are paged — arming (or disarming) a
// kill-switch is alert-worthy.
//
// Stub mode (no PolicyStore wired): returns a fixture-shaped success
// so the kind demo + dashboard local dev keep working, unless
// KUBEHERO_DEMO_MODE=false hard-disables fixtures — then the call
// fails with FailedPrecondition like the other fixture RPCs.
func (c *ControlPlane) ArmPolicy(
	ctx context.Context,
	req *connect.Request[kuberov1.ArmPolicyRequest],
) (*connect.Response[kuberov1.ArmPolicyResponse], error) {
	// Role hierarchy: admin = member + RegisterCluster + policy arming.
	if err := auth.Require(ctx, auth.RoleAdmin); err != nil {
		return nil, err
	}
	m := req.Msg
	name := strings.TrimSpace(m.GetPolicyName())
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("policy_name is required"))
	}

	actor := strings.TrimSpace(m.GetActor())
	if actor == "" {
		p := auth.PrincipalFromContext(ctx)
		actor = nonEmpty(p.Email, nonEmpty(p.Sub, "cli"))
	}
	reason := strings.TrimSpace(m.GetReason())
	action, outcome := "policy.disarm", "reverted"
	if m.GetArmed() {
		action, outcome = "policy.arm", "armed"
	}
	now := time.Now().UTC()

	if c.Policies == nil {
		if c.DemoFixturesDisabled {
			return nil, errDemoDisabled("ArmPolicy")
		}
		// Demo fixtures enabled: plausible fake success, shaped like the
		// real path so CLI demos and docs screenshots look right.
		return connect.NewResponse(&kuberov1.ArmPolicyResponse{
			PolicyName:      name,
			Armed:           m.GetArmed(),
			EffectiveAtUnix: now.Unix(),
			AuditId:         fmt.Sprintf("aud-%d", now.UnixNano()%1_000_000),
		}), nil
	}

	clusterID := strings.TrimSpace(m.GetClusterId())
	if clusterID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("cluster_id is required (UUID or slug)"))
	}

	pol, err := c.Policies.SetArmedByName(ctx, clusterID, name, m.GetArmed(), actor)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("set armed: %w", err))
	}

	// Audit row — AuditPG stamps the HMAC signature on Append, same as
	// every other writer. A failed audit write fails the RPC: an
	// unaudited kill-switch flip is worse than a retried one.
	auditID := fmt.Sprintf("aud-%d", now.UnixNano()%1_000_000)
	if c.Audit != nil {
		payload, _ := json.Marshal(map[string]any{
			"armed":  m.GetArmed(),
			"actor":  actor,
			"reason": reason,
		})
		id, err := c.Audit.Append(ctx, &store.AuditEntry{
			At:         now,
			OrgID:      strPtrAudit("default"),
			ClusterID:  strPtrAudit(clusterID),
			ActorSub:   actor,
			Action:     action,
			TargetKind: pol.Kind,
			TargetName: pol.Name,
			Payload:    payload,
			Outcome:    outcome,
		})
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit append: %w", err))
		}
		auditID = fmt.Sprintf("aud-%d", id)
	}

	// Best-effort page-out: the state change is already persisted and
	// audited, so a failing pager must not fail the arm itself.
	if c.Alerts != nil && len(c.AlertChannels) > 0 {
		state := "DISARMED"
		if m.GetArmed() {
			state = "ARMED"
		}
		msg := fmt.Sprintf("%s %s %s by %s", pol.Kind, pol.Name, state, actor)
		if reason != "" {
			msg += " — " + reason
		}
		_ = c.Alerts.Alert(ctx, c.AlertChannels, msg)
	}

	effective := now
	if pol.Armed && pol.ArmedAt != nil {
		effective = *pol.ArmedAt
	}
	return connect.NewResponse(&kuberov1.ArmPolicyResponse{
		PolicyName:      pol.Name,
		Armed:           pol.Armed,
		EffectiveAtUnix: effective.Unix(),
		AuditId:         auditID,
	}), nil
}
