// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rpc

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/pricing-engine/internal/pricing"
)

// Pricing is the in-process PricingService. Backed by Static sources today;
// real cloud scrapers slot in behind the pricing.Source interface.
type Pricing struct {
	sources map[pricing.Cloud]pricing.Source
}

func New() *Pricing {
	return &Pricing{
		sources: map[pricing.Cloud]pricing.Source{
			pricing.CloudAWS:   pricing.NewStatic(pricing.CloudAWS),
			pricing.CloudGCP:   pricing.NewStatic(pricing.CloudGCP),
			pricing.CloudAzure: pricing.NewStatic(pricing.CloudAzure),
		},
	}
}

var _ kuberov1connect.PricingServiceHandler = (*Pricing)(nil)

func (p *Pricing) Quote(
	ctx context.Context,
	req *connect.Request[kuberov1.QuoteRequest],
) (*connect.Response[kuberov1.QuoteResponse], error) {
	src, ok := p.sources[pricing.Cloud(req.Msg.GetCloud())]
	if !ok {
		return nil, connect.NewError(
			connect.CodeInvalidArgument,
			fmt.Errorf("unsupported cloud: %q", req.Msg.GetCloud()),
		)
	}
	q, err := src.Quote(ctx, req.Msg.GetSku(), req.Msg.GetRegion(), req.Msg.GetLifecycle())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&kuberov1.QuoteResponse{
		PricePerHour: q.PricePerHour,
		Currency:     q.Currency,
	}), nil
}
