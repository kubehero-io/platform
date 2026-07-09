// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rpc

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/pricing-engine/internal/pricing"
)

// Quoter is what the handler needs from the pricing layer. The serving
// path passes a *pricing.Catalog (cache + live sources + static
// fallback); tests can pass anything that quotes.
type Quoter interface {
	Quote(ctx context.Context, cloud pricing.Cloud, sku, region, lifecycle string) (pricing.Quote, error)
}

// Pricing is the in-process PricingService, backed by a Quoter.
type Pricing struct {
	quoter Quoter
}

func New(q Quoter) *Pricing {
	return &Pricing{quoter: q}
}

var _ kuberov1connect.PricingServiceHandler = (*Pricing)(nil)

func (p *Pricing) Quote(
	ctx context.Context,
	req *connect.Request[kuberov1.QuoteRequest],
) (*connect.Response[kuberov1.QuoteResponse], error) {
	cloud := pricing.Cloud(req.Msg.GetCloud())
	switch cloud {
	case pricing.CloudAWS, pricing.CloudGCP, pricing.CloudAzure:
	default:
		return nil, connect.NewError(
			connect.CodeInvalidArgument,
			fmt.Errorf("unsupported cloud: %q", req.Msg.GetCloud()),
		)
	}
	q, err := p.quoter.Quote(ctx, cloud, req.Msg.GetSku(), req.Msg.GetRegion(), req.Msg.GetLifecycle())
	if err != nil {
		switch {
		case errors.Is(err, pricing.ErrUnimplemented):
			return nil, connect.NewError(connect.CodeUnimplemented, err)
		case errors.Is(err, pricing.ErrNotFound):
			return nil, connect.NewError(connect.CodeNotFound, err)
		default:
			return nil, connect.NewError(connect.CodeUnavailable, err)
		}
	}
	return connect.NewResponse(&kuberov1.QuoteResponse{
		PricePerHour: q.PricePerHour,
		Currency:     q.Currency,
	}), nil
}
