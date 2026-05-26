// SPDX-License-Identifier: BUSL-1.1
package pricing

import (
	"context"
	"testing"
)

func TestStaticQuote(t *testing.T) {
	s := NewStatic(CloudAWS)
	q, err := s.Quote(context.Background(), "m5.large", "us-east-1", "on-demand")
	if err != nil {
		t.Fatal(err)
	}
	if q.PricePerHour != 0.096 {
		t.Fatalf("got %v want 0.096", q.PricePerHour)
	}
}
