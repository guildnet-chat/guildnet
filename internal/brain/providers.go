// SPDX-License-Identifier: AGPL-3.0-only

package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	aoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3"
	ooption "github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/ledger"
	"github.com/guildnet-chat/guildnet/models"
)

// Every model call an agent makes is one system prompt (maybe none) and one
// message, whatever provider serves the model. Claude models go through
// Claude's own API, which keeps prompt caching and full usage; every other
// provider through its OpenAI-compatible Chat Completions API, the one
// every provider speaks, with the few differences that matter handled here.

type call struct {
	model     models.Model
	system    string
	prompt    string
	maxTokens int64
	effort    string // low, medium, high, xhigh, max; "" for the model's default
	// A conversation with tools, in place of prompt: the turns so far, and
	// the tools the model may ask for.
	turns []turn
	tools []agent.Tool
}

type answer struct {
	text  string
	usage ledger.Usage
	cost  float64 // USD, when the provider says; 0 means work it out from usage
	// The tools the model asked for, if any, and its turn in the provider's
	// own form, to send back as it came.
	calls []toolCall
	raw   json.RawMessage
}

type caller interface {
	call(ctx context.Context, c call) (answer, error)
}

// callersFor makes a caller for each provider with a key; baseURLs, if set,
// override where a provider is reached (tests).
func callersFor(keys, baseURLs map[models.Provider]string) map[models.Provider]caller {
	out := map[models.Provider]caller{}
	for p, key := range keys {
		if key == "" {
			continue
		}
		base := baseURLs[p]
		if p == models.Anthropic {
			opts := []aoption.RequestOption{aoption.WithAPIKey(key)}
			if base != "" {
				opts = append(opts, aoption.WithBaseURL(base))
			}
			out[p] = claude{anthropic.NewClient(opts...)}
			continue
		}
		if base == "" {
			base = p.BaseURL()
		}
		out[p] = compatible{client: openai.NewClient(ooption.WithAPIKey(key), ooption.WithBaseURL(base)), provider: p}
	}
	return out
}

// claude calls Claude's Messages API.
type claude struct{ client anthropic.Client }

func (c claude) call(ctx context.Context, x call) (answer, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(x.model.Name),
		MaxTokens: x.maxTokens,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(x.prompt))},
	}
	if x.turns != nil {
		params.Messages = claudeMessages(x.turns)
		for _, t := range x.tools {
			params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
				Name: t.Name, Description: anthropic.String(t.Description),
				InputSchema: anthropic.ToolInputSchemaParam{Properties: t.Properties, Required: t.Required},
			}})
		}
	}
	if x.system != "" {
		// The system prompt is the same on every call for an agent, so it's
		// cached once it's past the model's minimum cacheable length.
		params.System = []anthropic.TextBlockParam{{Text: x.system, CacheControl: anthropic.NewCacheControlEphemeralParam()}}
	}
	if x.effort != "" {
		params.OutputConfig = anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(x.effort)}
	}
	resp, err := c.client.Messages.New(ctx, params)
	if err != nil {
		return answer{}, err
	}
	a := answer{text: strings.TrimSpace(textOf(resp)), usage: usageOf(resp)}
	for _, block := range resp.Content {
		if u, ok := block.AsAny().(anthropic.ToolUseBlock); ok {
			a.calls = append(a.calls, toolCallOf(u.ID, u.Name, string(u.Input)))
		}
	}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return a, fmt.Errorf("model declined to respond (%s)", resp.StopDetails.Category)
	}
	return a, nil
}

// compatible calls a provider's OpenAI-compatible Chat Completions API.
type compatible struct {
	client   openai.Client
	provider models.Provider
}

func (c compatible) call(ctx context.Context, x call) (answer, error) {
	if c.provider == models.OpenAI && x.turns != nil {
		return c.respond(ctx, x)
	}
	params := openai.ChatCompletionNewParams{Model: x.model.Name}
	if x.system != "" {
		params.Messages = append(params.Messages, openai.SystemMessage(x.system))
	}
	if x.turns != nil {
		params.Messages = append(params.Messages, compatibleMessages(x.turns)...)
		for _, t := range x.tools {
			params.Tools = append(params.Tools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
				Name: t.Name, Description: openai.String(t.Description),
				Parameters: shared.FunctionParameters{"type": "object", "properties": t.Properties, "required": nonNil(t.Required)},
			}))
		}
	} else {
		params.Messages = append(params.Messages, openai.UserMessage(x.prompt))
	}
	// OpenAI replaced max_tokens with max_completion_tokens for its reasoning
	// models; the others still take max_tokens.
	if c.provider == models.OpenAI {
		params.MaxCompletionTokens = openai.Int(x.maxTokens)
	} else {
		params.MaxTokens = openai.Int(x.maxTokens)
	}
	if x.effort != "" {
		params.ReasoningEffort = reasoningEffort(x.effort)
	}
	resp, err := c.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return answer{}, err
	}
	if len(resp.Choices) == 0 {
		return answer{}, errors.New("the model gave no answer")
	}
	choice := resp.Choices[0]
	a := answer{text: strings.TrimSpace(choice.Message.Content), usage: compatibleUsage(resp.Usage)}
	for _, tc := range choice.Message.ToolCalls {
		if tc.Type == "function" {
			a.calls = append(a.calls, toolCallOf(tc.ID, tc.Function.Name, tc.Function.Arguments))
		}
	}
	if len(a.calls) > 0 {
		a.raw = assistantTurn(choice.Message.RawJSON())
	}
	if a.usage == (ledger.Usage{}) {
		// Some providers don't say: charge an estimate, never nothing.
		a.usage = ledger.Usage{Input: estimateTokens(x.system + x.prompt + turnsText(x.turns)), Output: estimateTokens(choice.Message.Content)}
	}
	if f, ok := resp.Usage.JSON.ExtraFields["cost"]; ok {
		// OpenRouter says what the call cost.
		a.cost, _ = strconv.ParseFloat(f.Raw(), 64)
	}
	switch {
	case choice.Message.Refusal != "":
		return a, fmt.Errorf("model declined to respond: %s", choice.Message.Refusal)
	case choice.FinishReason == "content_filter":
		return a, errors.New("model declined to respond (content filter)")
	}
	return a, nil
}

// compatibleUsage reads usage the OpenAI way: prompt tokens include cached
// ones. Reasoning bills as output; OpenAI counts it in completion tokens,
// others (Gemini) apart from them, which the total shows.
func compatibleUsage(u openai.CompletionUsage) ledger.Usage {
	cached := min(u.PromptTokensDetails.CachedTokens, u.PromptTokens)
	out := ledger.Usage{Input: u.PromptTokens - cached, CacheRead: cached, Output: u.CompletionTokens}
	if r := u.CompletionTokensDetails.ReasoningTokens; r > 0 && u.TotalTokens >= u.PromptTokens+u.CompletionTokens+r {
		out.Output += r
	}
	return out
}

// estimateTokens errs high: about three bytes a token.
func estimateTokens(s string) int64 { return int64(len(s))/3 + 1 }

// reasoningEffort maps GuildNet's effort levels onto the OpenAI-style ones.
func reasoningEffort(e string) shared.ReasoningEffort {
	switch e {
	case "low":
		return shared.ReasoningEffortLow
	case "medium":
		return shared.ReasoningEffortMedium
	}
	return shared.ReasoningEffortHigh
}

func textOf(m *anthropic.Message) string {
	var out strings.Builder
	for _, block := range m.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			out.WriteString(t.Text)
		}
	}
	return out.String()
}

func usageOf(m *anthropic.Message) ledger.Usage {
	return ledger.Usage{
		Input:      m.Usage.InputTokens,
		Output:     m.Usage.OutputTokens,
		CacheWrite: m.Usage.CacheCreationInputTokens,
		CacheRead:  m.Usage.CacheReadInputTokens,
	}
}
