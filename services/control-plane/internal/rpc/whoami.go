// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rpc

import (
	"context"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
)

// WhoAmI reports the principal the auth interceptor resolved for this
// request. It never errors: an unauthenticated request that got this
// far was allowed through as anonymous.
func (c *ControlPlane) WhoAmI(
	ctx context.Context,
	_ *connect.Request[kuberov1.WhoAmIRequest],
) (*connect.Response[kuberov1.WhoAmIResponse], error) {
	p := auth.PrincipalFromContext(ctx)
	sub := p.Sub
	if sub == "" {
		sub = "anonymous"
	}
	return connect.NewResponse(&kuberov1.WhoAmIResponse{
		Subject:      sub,
		Role:         string(p.Role),
		Email:        p.Email,
		Groups:       p.Groups,
		ClusterId:    p.ClusterID,
		AuthRequired: c.AuthRequired,
	}), nil
}
