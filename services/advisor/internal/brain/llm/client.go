// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package llm

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// DefaultModel is the Claude model the advisor uses unless
// KUBEHERO_ADVISOR_MODEL says otherwise.
const DefaultModel = "claude-opus-5"

// defaultMaxTokens caps thinking + answer per turn. On Opus 5 thinking is
// on by default and counts against max_tokens, so this is generous; all
// calls stream, so a large cap never trips HTTP timeouts.
const defaultMaxTokens = 32000

// Config is shared by the briefing brain and the investigator.
type Config struct {
	// Model defaults to DefaultModel.
	Model string
	// MaxTokens defaults to defaultMaxTokens.
	MaxTokens int64
	// Options are extra client options (tests point BaseURL at a fake).
	Options []option.RequestOption
}

// ConfigFromEnv reads KUBEHERO_ADVISOR_MODEL. ANTHROPIC_API_KEY (and
// ANTHROPIC_BASE_URL) are read by the SDK itself.
func ConfigFromEnv() Config {
	return Config{Model: strings.TrimSpace(os.Getenv("KUBEHERO_ADVISOR_MODEL"))}
}

func (c Config) model() string {
	if c.Model != "" {
		return c.Model
	}
	return DefaultModel
}

func (c Config) maxTokens() int64 {
	if c.MaxTokens > 0 {
		return c.MaxTokens
	}
	return defaultMaxTokens
}

func (c Config) client() anthropic.Client {
	opts := append([]option.RequestOption{option.WithMaxRetries(2)}, c.Options...)
	return anthropic.NewClient(opts...)
}

// baseParams is every request's common shape:
//   - adaptive thinking (Opus 5's default, stated explicitly so a model
//     override keeps it) — no budget_tokens, no temperature/top_p/top_k,
//     no assistant prefill: all rejected on current models;
//   - server-side refusal fallbacks in "default" mode, so a request a
//     safety classifier declines is re-served by Anthropic's recommended
//     fallback model inside the same call;
//   - the system prompt cached (tools + system are the stable prefix).
func (c Config) baseParams(system string) anthropic.BetaMessageNewParams {
	return anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(c.model()),
		MaxTokens: c.maxTokens(),
		Thinking:  anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{}},
		System: []anthropic.BetaTextBlockParam{{
			Text:         system,
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		}},
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
		Fallbacks: anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()},
	}
}

// RefusalError reports a request the model (and its fallback chain)
// declined. Category is informational and may be empty.
type RefusalError struct {
	Category    string
	Explanation string
}

func (e *RefusalError) Error() string {
	cat := e.Category
	if cat == "" {
		cat = "unspecified"
	}
	return "model declined the request (refusal, category " + cat + ")"
}

// IsRefusal reports whether err is a RefusalError.
func IsRefusal(err error) (*RefusalError, bool) {
	var r *RefusalError
	ok := errors.As(err, &r)
	return r, ok
}

// checkStop turns non-answer stop reasons into errors. Branch on
// stop_reason, never on stop_details: details are optional even on a
// refusal, and content must not be trusted before this check.
func checkStop(msg *anthropic.BetaMessage) error {
	switch msg.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return &RefusalError{Category: string(msg.StopDetails.Category), Explanation: msg.StopDetails.Explanation}
	case anthropic.BetaStopReasonMaxTokens:
		return errors.New("response truncated at max_tokens")
	case anthropic.BetaStopReasonModelContextWindowExceeded:
		return errors.New("context window exceeded")
	case anthropic.BetaStopReasonToolUse, anthropic.BetaStopReasonPauseTurn:
		return fmt.Errorf("model still wanted to continue (stop_reason %s)", msg.StopReason)
	}
	return nil
}

// messageText concatenates the text blocks of a message in order. After
// a mid-stream fallback the partial text before the fallback block is
// the continuation context of the text after it, so both belong to the
// answer.
func messageText(msg *anthropic.BetaMessage) string {
	var b strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// servedBy names the model that produced msg and whether a server-side
// fallback served it (for logs; a fallback answer is still a good answer).
func servedBy(msg *anthropic.BetaMessage) (string, bool) {
	for _, it := range msg.Usage.Iterations {
		if it.Type == "fallback_message" {
			return string(msg.Model), true
		}
	}
	return string(msg.Model), false
}
