// SPDX-License-Identifier: BUSL-1.1
package rpc

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

func TestQuoteKnownSKU(t *testing.T) {
	r, err := New().Quote(context.Background(),
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

func TestQuoteUnknownCloud(t *testing.T) {
	_, err := New().Quote(context.Background(),
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
