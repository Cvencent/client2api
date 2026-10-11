package autoclaw

import (
	"encoding/json"
	"strings"

	"client2api/internal/core"
)

type chatBody struct {
	Model       string          `json:"model"`
	Messages    []chatMessage   `json:"messages"`
	Stream      bool            `json:"stream"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        []string        `json:"stop,omitempty"`
	Tools       []chatTool      `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
}

type chatMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content,omitempty"`
	Name             string         `json:"name,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type chatToolCall struct {
	ID       string                   `json:"id,omitempty"`
	Type     string                   `json:"type"`
	Function chatToolCallFunctionBody `json:"function"`
}

type chatToolCallFunctionBody struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func buildChatBody(req *core.ChatRequest, model string, stream bool) chatBody {
	body := chatBody{
		Model:       model,
		Messages:    make([]chatMessage, 0, len(req.Messages)),
		Stream:      stream,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
	}
	for _, m := range req.Messages {
		role := strings.TrimSpace(m.Role)
		if role == "" {
			role = "user"
		}
		// AutoClaw's managed proxy rejects OpenAI's `developer` role; its
		// desktop client maps that role onto `system` before sending.
		if role == "developer" {
			role = "system"
		}
		cm := chatMessage{
			Role:             role,
			Content:          messageContent(m),
			Name:             m.Name,
			ToolCallID:       m.ToolCallID,
			ReasoningContent: m.Reasoning,
		}
		for _, tc := range m.ToolCalls {
			typ := strings.TrimSpace(tc.Type)
			if typ == "" {
				typ = "function"
			}
			cm.ToolCalls = append(cm.ToolCalls, chatToolCall{
				ID:   tc.ID,
				Type: typ,
				Function: chatToolCallFunctionBody{
					Name:      tc.Name,
					Arguments: tc.Arguments,
				},
			})
		}
		body.Messages = append(body.Messages, cm)
	}
	for _, t := range req.Tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		typ := strings.TrimSpace(t.Type)
		if typ == "" {
			typ = "function"
		}
		body.Tools = append(body.Tools, chatTool{
			Type: typ,
			Function: chatFunction{
				Name:        name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		body.ToolChoice = req.ToolChoice
	}
	return body
}

func messageContent(m core.Message) any {
	if len(m.Parts) == 0 {
		return m.Content
	}
	parts := make([]map[string]any, 0, len(m.Parts)+1)
	if strings.TrimSpace(m.Content) != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	for _, p := range m.Parts {
		switch strings.ToLower(strings.TrimSpace(p.Type)) {
		case "", "text":
			if p.Text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": p.Text})
			}
		case "image_url", "image":
			url := strings.TrimSpace(p.ImageURL)
			if url == "" {
				continue
			}
			img := map[string]any{"url": url}
			if d := strings.TrimSpace(p.Detail); d != "" {
				img["detail"] = d
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": img})
		}
	}
	if len(parts) == 0 {
		return m.Content
	}
	return parts
}
