// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rpc

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
)

func ingestCode(err error) connect.Code {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code()
	}
	return connect.CodeUnknown
}

func TestIngestPodCostLimitsAndScoping(t *testing.T) {
	svc := New()
	member := auth.WithPrincipal(context.Background(), auth.Principal{Sub: "m", Role: auth.RoleMember})
	scoped := auth.WithPrincipal(context.Background(), auth.Principal{Sub: "cluster:u1", Role: auth.RoleMember, ClusterID: "u1"})

	tooMany := make([]*kuberov1.PodCostSample, maxPodCostSamples+1)
	if _, err := svc.IngestPodCost(member, connect.NewRequest(&kuberov1.IngestPodCostRequest{ClusterId: "c", Samples: tooMany})); ingestCode(err) != connect.CodeInvalidArgument {
		t.Fatalf("over sample limit: %v", err)
	}
	tooManyNodes := make([]*kuberov1.NodeCostSample, maxNodeSamples+1)
	if _, err := svc.IngestPodCost(member, connect.NewRequest(&kuberov1.IngestPodCostRequest{ClusterId: "c", Nodes: tooManyNodes})); ingestCode(err) != connect.CodeInvalidArgument {
		t.Fatalf("over node limit: %v", err)
	}
	one := []*kuberov1.PodCostSample{{Pod: "p", CostUsdSec: 1}}
	if _, err := svc.IngestPodCost(scoped, connect.NewRequest(&kuberov1.IngestPodCostRequest{ClusterId: "u2", Samples: one})); ingestCode(err) != connect.CodePermissionDenied {
		t.Fatalf("scoped token writing another cluster: %v", err)
	}
	if _, err := svc.IngestPodCost(member, connect.NewRequest(&kuberov1.IngestPodCostRequest{ClusterId: "no spaces allowed", Samples: one})); ingestCode(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid cluster id: %v", err)
	}
	// Stub mode reports samples and node rows as dropped.
	res, err := svc.IngestPodCost(scoped, connect.NewRequest(&kuberov1.IngestPodCostRequest{
		Samples: one, Nodes: []*kuberov1.NodeCostSample{{Node: "n1"}},
	}))
	if err != nil || res.Msg.GetDropped() != 2 || res.Msg.GetWritten() != 0 {
		t.Fatalf("stub mode: %v %v", res, err)
	}
	// An empty request is a no-op, not an error.
	if res, err := svc.IngestPodCost(member, connect.NewRequest(&kuberov1.IngestPodCostRequest{})); err != nil || res.Msg.GetDropped() != 0 {
		t.Fatalf("empty: %v %v", res, err)
	}
}
