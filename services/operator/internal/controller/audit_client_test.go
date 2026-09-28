// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
)

type auditSink struct {
	kuberov1connect.UnimplementedControlPlaneServiceHandler
	got  *kuberov1.AppendAuditEntryRequest
	auth string
}

func (a *auditSink) AppendAuditEntry(
	_ context.Context,
	req *connect.Request[kuberov1.AppendAuditEntryRequest],
) (*connect.Response[kuberov1.AppendAuditEntryResponse], error) {
	a.got = req.Msg
	a.auth = req.Header().Get("Authorization")
	return connect.NewResponse(&kuberov1.AppendAuditEntryResponse{Id: 7}), nil
}

// TestHTTPAuditEmitterSpeaksConnectJSON sends events through the real
// generated handler (protojson codec): the payload must survive as the
// original JSON bytes.
func TestHTTPAuditEmitterSpeaksConnectJSON(t *testing.T) {
	sink := &auditSink{}
	mux := http.NewServeMux()
	mux.Handle(kuberov1connect.NewControlPlaneServiceHandler(sink))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	em := &HTTPAuditEmitter{Endpoint: srv.URL, Token: "op-token"}
	err := em.Emit(context.Background(), AuditEvent{
		Action: "rightsize.apply", TargetKind: "Deployment", TargetName: "payments/api",
		ActorSub: "operator", ClusterID: "eks-1", Outcome: "applied", EffectUsdMonth: 120.5,
		PayloadJSON: map[string]any{"changeId": "rs-1a2b3c4d", "previousSpec": map[string]any{"cpu": "2"}},
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if sink.got.GetAction() != "rightsize.apply" || sink.got.GetTargetName() != "payments/api" ||
		sink.got.GetEffectUsdMonth() != 120.5 || sink.got.GetClusterId() != "eks-1" {
		t.Errorf("fields lost: %+v", sink.got)
	}
	var payload map[string]any
	if err := json.Unmarshal(sink.got.GetPayload(), &payload); err != nil {
		t.Fatalf("payload is not the original JSON: %v (%q)", err, sink.got.GetPayload())
	}
	if payload["changeId"] != "rs-1a2b3c4d" {
		t.Errorf("payload = %v", payload)
	}
	if sink.auth != "Bearer op-token" {
		t.Errorf("Authorization = %q", sink.auth)
	}

	// Raw payload bytes pass through unchanged.
	if err := em.Emit(context.Background(), AuditEvent{Action: "ceiling.tripped", Payload: []byte(`{"burnRateMilli":2000}`)}); err != nil {
		t.Fatal(err)
	}
	if string(sink.got.GetPayload()) != `{"burnRateMilli":2000}` {
		t.Errorf("raw payload = %q", sink.got.GetPayload())
	}
}
