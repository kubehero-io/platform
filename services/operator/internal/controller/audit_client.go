// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AuditEmitter posts a single audit entry into the control-plane.
//
// We deliberately avoid pulling the generated Connect bindings into the
// operator module; the wire format is just JSON to a stable URL, and a
// 60-line client keeps the operator's dependency graph tight.
type AuditEmitter interface {
	Emit(ctx context.Context, e AuditEvent) error
}

// AuditEvent is the operator-side shape; field names match the proto
// camelCase wire format so the JSON body needs no extra translation.
type AuditEvent struct {
	Org            string  `json:"org,omitempty"`
	ClusterID      string  `json:"clusterId,omitempty"`
	ActorSub       string  `json:"actorSub,omitempty"`
	ActorEmail     string  `json:"actorEmail,omitempty"`
	Action         string  `json:"action"`
	TargetKind     string  `json:"targetKind,omitempty"`
	TargetName     string  `json:"targetName,omitempty"`
	Payload        []byte  `json:"-"`
	PayloadJSON    any     `json:"payload,omitempty"`
	Outcome        string  `json:"outcome,omitempty"`
	EffectUsdMonth float64 `json:"effectUsdMonth,omitempty"`
}

// HTTPAuditEmitter is the production emitter. Endpoint is the
// control-plane base URL (no trailing slash); when empty, every Emit
// is a logged no-op so the operator never blocks on a missing cp.
type HTTPAuditEmitter struct {
	Endpoint string
	Token    string         // optional Bearer for future RBAC
	Client   *http.Client   // defaulted on first use
}

// NoopAuditEmitter is the test/stub default — never errors, never calls.
type NoopAuditEmitter struct{}

func (NoopAuditEmitter) Emit(_ context.Context, _ AuditEvent) error { return nil }

func (h *HTTPAuditEmitter) Emit(ctx context.Context, e AuditEvent) error {
	if h.Endpoint == "" {
		return nil // stub mode
	}
	if h.Client == nil {
		h.Client = &http.Client{Timeout: 5 * time.Second}
	}
	if e.Action == "" {
		return errors.New("AuditEvent.Action is required")
	}

	// Caller may set either raw bytes or a JSON-marshalable struct.
	if len(e.Payload) > 0 && e.PayloadJSON == nil {
		e.PayloadJSON = json.RawMessage(e.Payload)
	}

	body, err := json.Marshal(e)
	if err != nil {
		return err
	}

	url := strings.TrimRight(h.Endpoint, "/") +
		"/kubehero.v1.ControlPlaneService/AppendAuditEntry"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return fmt.Errorf("audit emit: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("audit emit: http %d · %s",
			resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}
