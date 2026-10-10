// SPDX-License-Identifier: AGPL-3.0-only

package brain

import (
	"encoding/json"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
)

// A conversation with tools is kept in no provider's shape, so a paused one
// can be kept (in an approval's token) and carried on later, even if the
// agent has moved to another provider's model since.

// turn is one message in a conversation with tools.
type turn struct {
	Role    string     `json:"role"` // user, assistant, or tool
	Text    string     `json:"text,omitempty"`
	Calls   []toolCall `json:"calls,omitempty"`   // assistant: the tools it asked for
	CallID  string     `json:"call_id,omitempty"` // tool: the call it answers
	IsError bool       `json:"error,omitempty"`   // tool: the call failed
	// Raw is an assistant turn as an OpenAI-compatible provider gave it, sent
	// back as it came: Gemini's thought signatures ride in it.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// toolCall is a tool the model asked for. Bad is set when its input wasn't
// a JSON object, which the model is told.
type toolCall struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input,omitempty"`
	Args  string         `json:"args,omitempty"` // the input as the model wrote it
	Bad   bool           `json:"bad,omitempty"`
}

func toolCallOf(id, name, args string) toolCall {
	c := toolCall{ID: id, Name: name, Args: args}
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	if err := json.Unmarshal([]byte(args), &c.Input); err != nil || c.Input == nil {
		c.Bad, c.Input = true, nil
	}
	return c
}

func turnsText(turns []turn) string {
	var b strings.Builder
	for _, t := range turns {
		b.WriteString(t.Text)
		for _, c := range t.Calls {
			b.WriteString(c.Args)
		}
	}
	return b.String()
}

// claudeMessages is the conversation for Claude's Messages API: a tool's
// results go back in the next user message.
func claudeMessages(turns []turn) []anthropic.MessageParam {
	var out []anthropic.MessageParam
	var results []anthropic.ContentBlockParamUnion
	flush := func() {
		if len(results) > 0 {
			out = append(out, anthropic.NewUserMessage(results...))
			results = nil
		}
	}
	for _, t := range turns {
		switch t.Role {
		case "tool":
			results = append(results, anthropic.NewToolResultBlock(t.CallID, t.Text, t.IsError))
		case "assistant":
			flush()
			var blocks []anthropic.ContentBlockParamUnion
			if t.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(t.Text))
			}
			for _, c := range t.Calls {
				in := c.Input
				if in == nil {
					in = map[string]any{}
				}
				blocks = append(blocks, anthropic.NewToolUseBlock(c.ID, in, c.Name))
			}
			out = append(out, anthropic.NewAssistantMessage(blocks...))
		default:
			flush()
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(t.Text)))
		}
	}
	flush()
	return out
}

// compatibleMessages is the conversation for Chat Completions. An assistant
// turn goes back as the provider gave it, when it did.
func compatibleMessages(turns []turn) []openai.ChatCompletionMessageParamUnion {
	var out []openai.ChatCompletionMessageParamUnion
	for _, t := range turns {
		switch t.Role {
		case "tool":
			out = append(out, openai.ToolMessage(t.Text, t.CallID))
		case "assistant":
			var a openai.ChatCompletionAssistantMessageParam
			if len(t.Raw) > 0 {
				a = param.Override[openai.ChatCompletionAssistantMessageParam](t.Raw)
			} else {
				if t.Text != "" {
					a.Content.OfString = openai.String(t.Text)
				}
				for _, c := range t.Calls {
					args := c.Args
					if args == "" {
						b, _ := json.Marshal(c.Input)
						args = string(b)
					}
					a.ToolCalls = append(a.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: c.ID, Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{Name: c.Name, Arguments: args},
					}})
				}
			}
			out = append(out, openai.ChatCompletionMessageParamUnion{OfAssistant: &a})
		default:
			out = append(out, openai.UserMessage(t.Text))
		}
	}
	return out
}

// assistantTurn is an assistant message from a Chat Completions answer, as
// it can be sent back: its role, words, and tool calls (with whatever the
// provider put in them, like Gemini's thought signatures), and OpenRouter's
// reasoning details; not the answer-only fields a request would refuse.
func assistantTurn(raw string) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &m) != nil {
		return nil
	}
	keep := map[string]json.RawMessage{"role": json.RawMessage(`"assistant"`)}
	for _, k := range []string{"content", "tool_calls", "reasoning_details"} {
		if v, ok := m[k]; ok && string(v) != "null" {
			keep[k] = v
		}
	}
	b, _ := json.Marshal(keep)
	return b
}

// nonNil is an empty list, not null, for a schema's required.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
