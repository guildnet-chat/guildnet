// SPDX-License-Identifier: AGPL-3.0-only

package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/guildnet-chat/guildnet/ledger"
)

// OpenAI's reasoning models take tools only through its Responses API, not
// Chat Completions, so an OpenAI conversation with tools goes there. Nothing
// is kept at OpenAI (store: false): each answer's items, including its
// reasoning (encrypted), go back with the conversation, so the model keeps
// its train of thought between tool calls, and a paused conversation carries
// on from an approval's token.

func (c compatible) respond(ctx context.Context, x call) (answer, error) {
	params := responses.ResponseNewParams{
		Model:           x.model.Name,
		MaxOutputTokens: openai.Int(x.maxTokens),
		Store:           openai.Bool(false),
		Include:         []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent},
	}
	if x.system != "" {
		params.Instructions = openai.String(x.system)
	}
	if x.effort != "" {
		params.Reasoning = shared.ReasoningParam{Effort: reasoningEffort(x.effort)}
	}
	for _, t := range x.tools {
		params.Tools = append(params.Tools, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
			Name: t.Name, Description: openai.String(t.Description), Strict: openai.Bool(false),
			Parameters: map[string]any{"type": "object", "properties": t.Properties, "required": nonNil(t.Required)},
		}})
	}
	params.SetExtraFields(map[string]any{"input": responsesInput(x.turns)})
	resp, err := c.client.Responses.New(ctx, params)
	if err != nil {
		return answer{}, err
	}
	var out struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal([]byte(resp.RawJSON()), &out); err != nil {
		return answer{}, fmt.Errorf("reading OpenAI's answer: %w", err)
	}
	u := resp.Usage
	cached := min(u.InputTokensDetails.CachedTokens, u.InputTokens)
	a := answer{text: strings.TrimSpace(resp.OutputText()), usage: ledger.Usage{Input: u.InputTokens - cached, CacheRead: cached, Output: u.OutputTokens}}
	var refusal string
	for _, item := range out.Output {
		var head struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type    string `json:"type"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		}
		json.Unmarshal(item, &head)
		switch head.Type {
		case "function_call":
			a.calls = append(a.calls, toolCallOf(head.CallID, head.Name, head.Arguments))
		case "message":
			for _, c := range head.Content {
				if c.Type == "refusal" {
					refusal = c.Refusal
				}
			}
		}
	}
	if len(a.calls) > 0 {
		a.raw, _ = json.Marshal(out.Output)
	}
	if refusal != "" {
		return a, fmt.Errorf("model declined to respond: %s", refusal)
	}
	if len(a.calls) == 0 && a.text == "" && resp.Status == "incomplete" {
		return a, errors.New("the model ran out of room before answering")
	}
	return a, nil
}

// responsesInput is the conversation as Responses API input items. An
// assistant turn that came from this API goes back as its own items; one
// from elsewhere (the agent was on another provider) is rebuilt.
func responsesInput(turns []turn) []any {
	var out []any
	for _, t := range turns {
		switch t.Role {
		case "tool":
			out = append(out, map[string]any{"type": "function_call_output", "call_id": t.CallID, "output": t.Text})
		case "assistant":
			var items []json.RawMessage
			if json.Unmarshal(t.Raw, &items) == nil && len(items) > 0 {
				for _, it := range items {
					out = append(out, it)
				}
				continue
			}
			if t.Text != "" {
				out = append(out, map[string]any{"role": "assistant", "content": t.Text})
			}
			for _, c := range t.Calls {
				args := c.Args
				if args == "" {
					b, _ := json.Marshal(c.Input)
					args = string(b)
				}
				out = append(out, map[string]any{"type": "function_call", "call_id": c.ID, "name": c.Name, "arguments": args})
			}
		default:
			out = append(out, map[string]any{"role": "user", "content": t.Text})
		}
	}
	return out
}
