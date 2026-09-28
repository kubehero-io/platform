// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package investigate answers free-form operator questions ("why did
// checkout's spend jump last night?") by running READ-ONLY tools over
// the control plane's signals — cost, logs, profiles, network, alerts,
// rightsizing — and returning an answer grounded in cited evidence plus
// guarded proposals.
//
// Two investigators share one Toolbox (validation, call budget, step
// recording, result truncation):
//   - Rules: deterministic keyword + subject planning, used offline and
//     as the fallback whenever the LLM path fails or refuses.
//   - the LLM investigator (internal/brain/llm), an agentic tool loop.
//
// Neither can do anything but read: the Toolbox only reaches
// backend.Backend, which exposes no mutation RPCs, and every proposed
// action goes through brain.ValidateActions.
package investigate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

// Request limits (validated before any tool runs).
const (
	MaxQuestionLen  = 2000
	MaxContextLen   = 512
	MaxClusterIDLen = 128
)

// Request is a validated, normalised investigation question.
type Request struct {
	Question  string
	ClusterID string
	Window    string // 1h | 24h | 7d
	Context   string // dashboard path or anomaly id the question came from
}

// ErrInvalid wraps request validation failures (→ InvalidArgument).
var ErrInvalid = errors.New("invalid investigation request")

// NormalizeRequest validates the wire request: question required and
// bounded, window one of 1h|24h|7d (default 24h), control characters
// stripped from every free-text field.
func NormalizeRequest(r *kuberov1.InvestigateRequest) (Request, error) {
	q := cleanText(r.GetQuestion())
	switch {
	case q == "":
		return Request{}, fmt.Errorf("%w: question is required", ErrInvalid)
	case len([]rune(q)) > MaxQuestionLen:
		return Request{}, fmt.Errorf("%w: question exceeds %d characters", ErrInvalid, MaxQuestionLen)
	}
	window := strings.TrimSpace(r.GetWindow())
	switch window {
	case "":
		window = "24h"
	case "1h", "24h", "7d":
	default:
		return Request{}, fmt.Errorf("%w: window must be 1h, 24h or 7d", ErrInvalid)
	}
	cluster := strings.TrimSpace(r.GetClusterId())
	if len(cluster) > MaxClusterIDLen || strings.ContainsFunc(cluster, unicode.IsSpace) {
		return Request{}, fmt.Errorf("%w: cluster_id is malformed", ErrInvalid)
	}
	ctxText := cleanText(r.GetContext())
	if len([]rune(ctxText)) > MaxContextLen {
		return Request{}, fmt.Errorf("%w: context exceeds %d characters", ErrInvalid, MaxContextLen)
	}
	return Request{Question: q, ClusterID: cluster, Window: window, Context: ctxText}, nil
}

// cleanText trims and drops control characters (keeping newlines/tabs as
// spaces) so free text can't smuggle terminal escapes into logs or UIs.
func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// Key is the cache key for a request.
func (r Request) Key() string {
	return strings.ToLower(r.Question) + "\x00" + r.ClusterID + "\x00" + r.Window + "\x00" + r.Context
}

// Emitter receives live events (tool steps, progress notes). It may be
// called from several goroutines; implementations serialise.
type Emitter func(*kuberov1.InvestigateStreamResponse)

func (e Emitter) progress(msg string) {
	if e != nil {
		e(&kuberov1.InvestigateStreamResponse{Event: &kuberov1.InvestigateStreamResponse_Progress{Progress: msg}})
	}
}

func (e Emitter) step(s *kuberov1.InvestigateStep) {
	if e != nil {
		e(&kuberov1.InvestigateStreamResponse{Event: &kuberov1.InvestigateStreamResponse_Step{Step: s}})
	}
}

// Progress emits a short progress note (safe on a nil Emitter).
func (e Emitter) Progress(msg string) { e.progress(msg) }

// Investigator answers one question.
type Investigator interface {
	Investigate(ctx context.Context, req Request, emit Emitter) (*kuberov1.InvestigateResponse, error)
}

// NewID returns a response id like "inv-3f9a1c2e".
func NewID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("inv-%08x", uint32(time.Now().UnixNano()))
	}
	return "inv-" + hex.EncodeToString(b)
}
