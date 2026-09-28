// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package llm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
)

// fakeAnthropic implements just enough of POST /v1/messages (streaming
// SSE) for the Go SDK: each request is answered by the next scripted
// turn, and every request body + beta header is recorded for assertions.
type fakeAnthropic struct {
	*httptest.Server
	mu       sync.Mutex
	turns    []fakeTurn
	requests []map[string]any
	betas    []string
}

// fakeTurn is one assistant turn: tool calls, or final text, or a
// refusal; status != 0 returns an API error instead.
type fakeTurn struct {
	toolCalls []fakeToolCall
	text      string
	refusal   string // refusal category
	status    int
}

type fakeToolCall struct {
	name  string
	input string // raw JSON
}

func newFakeAnthropic(t *testing.T, turns ...fakeTurn) *fakeAnthropic {
	t.Helper()
	f := &fakeAnthropic{turns: turns}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// config points an llm.Config at the fake.
func (f *fakeAnthropic) config() Config {
	return Config{Options: []option.RequestOption{
		option.WithBaseURL(f.URL), option.WithAPIKey("test-key"), option.WithMaxRetries(0),
	}}
}

func (f *fakeAnthropic) request(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

func (f *fakeAnthropic) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeAnthropic) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)

	f.mu.Lock()
	idx := len(f.requests)
	f.requests = append(f.requests, req)
	f.betas = append(f.betas, r.Header.Get("anthropic-beta"))
	var turn fakeTurn
	if idx < len(f.turns) {
		turn = f.turns[idx]
	} else {
		turn = fakeTurn{status: http.StatusBadRequest}
	}
	f.mu.Unlock()

	if turn.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(turn.status)
		_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"scripted failure"}}`)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	}
	model, _ := req["model"].(string)
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": fmt.Sprintf("msg_%d", idx), "type": "message", "role": "assistant", "model": model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "stop_details": nil,
		"usage": map[string]any{"input_tokens": 100, "output_tokens": 1},
	}})
	stop, details := "end_turn", any(nil)
	i := 0
	switch {
	case turn.refusal != "":
		stop = "refusal"
		details = map[string]any{"type": "refusal", "category": turn.refusal, "explanation": "declined by policy"}
	case len(turn.toolCalls) > 0:
		stop = "tool_use"
		for _, tc := range turn.toolCalls {
			send("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{
				"type": "tool_use", "id": fmt.Sprintf("toolu_%d_%d", idx, i), "name": tc.name, "input": map[string]any{}}})
			// Split the JSON across two deltas like the real API does.
			half := len(tc.input) / 2
			for _, part := range []string{tc.input[:half], tc.input[half:]} {
				send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": part}})
			}
			send("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
			i++
		}
	default:
		send("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""}})
		for _, chunk := range chunks(turn.text, 64) {
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": chunk}})
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	}
	send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil, "stop_details": details},
		"usage": map[string]any{"output_tokens": 50}})
	send("message_stop", map[string]any{"type": "message_stop"})
}

func chunks(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}

// toolResults returns the tool_result blocks of the last user message in
// a recorded request.
func toolResults(req map[string]any) []map[string]any {
	msgs, _ := req["messages"].([]any)
	var out []map[string]any
	if len(msgs) == 0 {
		return out
	}
	last, _ := msgs[len(msgs)-1].(map[string]any)
	content, _ := last["content"].([]any)
	for _, c := range content {
		if m, ok := c.(map[string]any); ok && m["type"] == "tool_result" {
			out = append(out, m)
		}
	}
	return out
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func hasBeta(header, beta string) bool {
	for _, b := range strings.Split(header, ",") {
		if strings.TrimSpace(b) == beta {
			return true
		}
	}
	return false
}
