// SPDX-License-Identifier: BUSL-1.1
package rpc

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/pricing-engine/internal/pricing"
)

// newStaticOnly builds a Pricing handler backed by a catalog with no
// live sources — every quote resolves from the static table, matching
// the pre-catalog behavior these tests were written against.
func newStaticOnly() *Pricing {
	return New(pricing.NewCatalog(0, slog.New(slog.NewTextHandler(io.Discard, nil))))
}

func TestQuoteKnownSKU(t *testing.T) {
	r, err := newStaticOnly().Quote(context.Background(),
		connect.NewRequest(&kuberov1.QuoteRequest{
			Cloud:     "aws",
			Sku:       "m5.large",
			Region:    "us-east-1",
			Lifecycle: "on-demand",
		}))
	if err != nil {
		t.Fatal(err)
	}
	if r.Msg.GetPricePerHour() != 0.096 {
		t.Fatalf("price %v want 0.096", r.Msg.GetPricePerHour())
	}
}

func TestQuoteUnknownSKU(t *testing.T) {
	_, err := newStaticOnly().Quote(context.Background(),
		connect.NewRequest(&kuberov1.QuoteRequest{
			Cloud:     "aws",
			Sku:       "z9.mega",
			Region:    "us-east-1",
			Lifecycle: "on-demand",
		}))
	if err == nil {
		t.Fatal("expected error for unknown SKU")
	}
	if ce := new(connect.Error); !asConnectErr(err, ce) || ce.Code() != connect.CodeNotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestQuoteUnknownCloud(t *testing.T) {
	_, err := newStaticOnly().Quote(context.Background(),
		connect.NewRequest(&kuberov1.QuoteRequest{Cloud: "bogus"}))
	if err == nil {
		t.Fatal("expected error for bogus cloud")
	}
	if ce := new(connect.Error); !asConnectErr(err, ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func asConnectErr(err error, target *connect.Error) bool {
	if ce, ok := err.(*connect.Error); ok {
		*target = *ce
		return true
	}
	return false
}
