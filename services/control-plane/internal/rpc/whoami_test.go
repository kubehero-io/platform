// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rpc

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
)

func TestWhoAmIEchoesPrincipal(t *testing.T) {
	cp := New(Options{AuthRequired: true})
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Sub: "cluster:c-9", Role: auth.RoleMember, ClusterID: "c-9",
	})
	res, err := cp.WhoAmI(ctx, connect.NewRequest(&kuberov1.WhoAmIRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	m := res.Msg
	if m.GetSubject() != "cluster:c-9" || m.GetRole() != "member" || m.GetClusterId() != "c-9" || !m.GetAuthRequired() {
		t.Fatalf("got %+v", m)
	}
}

func TestWhoAmIAnonymous(t *testing.T) {
	res, err := New().WhoAmI(context.Background(), connect.NewRequest(&kuberov1.WhoAmIRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetSubject() != "anonymous" || res.Msg.GetRole() != "anonymous" {
		t.Fatalf("got %+v", res.Msg)
	}
}
