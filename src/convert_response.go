package main

// 非流式响应转换：上游响应 -> canonResponse -> 调用方协议。

import (
	"encoding/json"
	"fmt"
	"time"
)

type chatResponseIn struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Message struct {
			Content          json.RawMessage   `json:"content"`
			Refusal          string            `json:"refusal"`
			ReasoningContent json.RawMessage   `json:"reasoning_content"`
			Reasoning        json.RawMessage   `json:"reasoning"`
			ToolCalls        []chatToolCallIn  `json:"tool_calls"`
			FunctionCall     *chatFunctionCall `json:"function_call"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

type responsesOutputItemIn struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
}

type responsesResponseIn struct {
	ID                string `json:"id"`
	Model             string `json:"model"`
	CreatedAt         int64  `json:"created_at"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []responsesOutputItemIn `json:"output"`
	Usage  *responsesUsage         `json:"usage"`
}

type anthropicBlockIn struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

type anthropicResponseIn struct {
	ID         string             `json:"id"`
	Model      string             `json:"model"`
	Content    []anthropicBlockIn `json:"content"`
	StopReason *string            `json:"stop_reason"`
	Usage      *anthropicUsage    `json:"usage"`
}

func parseCanonicalResponse(proto Protocol, raw []byte) (*canonResponse, error) {
	switch proto {
	case protoChat:
		return parseChatResponse(raw)
	case protoResponses:
		return parseResponsesResponse(raw)
	case protoAnthropic:
		return parseAnthropicResponse(raw)
	}
	return nil, fmt.Errorf("unsupported protocol %q", proto)
}

func parseChatResponse(raw []byte) (*canonResponse, error) {
	var in chatResponseIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	resp := &canonResponse{ID: in.ID, Model: in.Model, Created: in.Created, StopReason: stopEndTurn}
	if usage, ok := usageFromChat(in.Usage); ok {
		resp.Usage = &usage
	}
	if len(in.Choices) == 0 {
		return resp, nil
	}
	msg := in.Choices[0].Message
	resp.Parts = append(resp.Parts, parseChatContent(msg.Content)...)
	if msg.Refusal != "" {
		resp.Parts = append(resp.Parts, canonPart{Kind: partText, Text: msg.Refusal})
	}
	reasoning := rawText(msg.ReasoningContent)
	if reasoning == "" {
		reasoning = rawText(msg.Reasoning)
	}
	if reasoning != "" {
		resp.Parts = append(resp.Parts, canonPart{Kind: partReasoning, Text: reasoning})
	}
	for _, tc := range msg.ToolCalls {
		resp.Parts = append(resp.Parts, canonPart{
			Kind:      partToolUse,
			ToolID:    tc.ID,
			ToolName:  tc.Function.Name,
			ToolInput: normalizeToolInput(json.RawMessage(tc.Function.Arguments)),
		})
	}
	if msg.FunctionCall != nil {
		resp.Parts = append(resp.Parts, canonPart{
			Kind:      partToolUse,
			ToolName:  msg.FunctionCall.Name,
			ToolInput: normalizeToolInput(json.RawMessage(msg.FunctionCall.Arguments)),
		})
	}
	resp.StopReason = mapChatFinish(in.Choices[0].FinishReason)
	if hasPart(resp.Parts, partToolUse) && resp.StopReason == stopEndTurn {
		resp.StopReason = stopToolUse
	}
	return resp, nil
}

func parseResponsesResponse(raw []byte) (*canonResponse, error) {
	var in responsesResponseIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	resp := &canonResponse{ID: in.ID, Model: in.Model, Created: in.CreatedAt, StopReason: stopEndTurn}
	if usage, ok := usageFromResponses(in.Usage); ok {
		resp.Usage = &usage
	}
	for _, item := range in.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				switch c.Type {
				case "output_text", "text", "input_text":
					if c.Text != "" {
						resp.Parts = append(resp.Parts, canonPart{Kind: partText, Text: c.Text})
					}
				case "refusal":
					if c.Refusal != "" {
						resp.Parts = append(resp.Parts, canonPart{Kind: partText, Text: c.Refusal})
					}
				}
			}
		case "function_call":
			callID := item.CallID
			if callID == "" {
				callID = item.ID
			}
			resp.Parts = append(resp.Parts, canonPart{
				Kind:      partToolUse,
				ToolID:    callID,
				ToolName:  item.Name,
				ToolInput: responsesArguments(item.Arguments),
			})
		case "reasoning":
			var texts []string
			for _, s := range item.Summary {
				if s.Text != "" {
					texts = append(texts, s.Text)
				}
			}
			for _, c := range item.Content {
				if c.Text != "" {
					texts = append(texts, c.Text)
				}
			}
			if len(texts) > 0 {
				resp.Parts = append(resp.Parts, canonPart{Kind: partReasoning, Text: joinStrings(texts, "")})
			}
		}
	}
	if hasPart(resp.Parts, partToolUse) {
		resp.StopReason = stopToolUse
	}
	if in.Status == "incomplete" && in.IncompleteDetails != nil {
		switch in.IncompleteDetails.Reason {
		case "max_output_tokens":
			resp.StopReason = stopMaxTokens
		case "content_filter":
			resp.StopReason = stopRefusal
		}
	}
	return resp, nil
}

func responsesArguments(raw json.RawMessage) json.RawMessage {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return normalizeToolInput(json.RawMessage(s))
	}
	return normalizeToolInput(raw)
}

func parseAnthropicResponse(raw []byte) (*canonResponse, error) {
	var in anthropicResponseIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	resp := &canonResponse{ID: in.ID, Model: in.Model, Created: time.Now().Unix()}
	if usage, ok := usageFromAnthropic(in.Usage); ok {
		resp.Usage = &usage
	}
	for _, b := range in.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				resp.Parts = append(resp.Parts, canonPart{Kind: partText, Text: b.Text})
			}
		case "thinking":
			if b.Thinking != "" {
				resp.Parts = append(resp.Parts, canonPart{Kind: partReasoning, Text: b.Thinking})
			}
		case "tool_use":
			resp.Parts = append(resp.Parts, canonPart{
				Kind:      partToolUse,
				ToolID:    b.ID,
				ToolName:  b.Name,
				ToolInput: normalizeToolInput(b.Input),
			})
		}
	}
	resp.StopReason = anthropicStop(derefString(in.StopReason))
	if hasPart(resp.Parts, partToolUse) && resp.StopReason == stopEndTurn {
		resp.StopReason = stopToolUse
	}
	return resp, nil
}

// extractUsageFromBody 仅用于透传路径：尽力从上游响应体里取用量。
func extractUsageFromBody(proto Protocol, raw []byte) *canonUsage {
	resp, err := parseCanonicalResponse(proto, raw)
	if err != nil || resp == nil {
		return nil
	}
	return resp.Usage
}

func renderCanonicalResponse(proto Protocol, resp *canonResponse) ([]byte, error) {
	switch proto {
	case protoChat:
		return renderChatResponse(resp)
	case protoResponses:
		return renderResponsesResponse(resp)
	case protoAnthropic:
		return renderAnthropicResponse(resp)
	}
	return nil, fmt.Errorf("unsupported protocol %q", proto)
}

func renderChatResponse(resp *canonResponse) ([]byte, error) {
	msg := map[string]any{"role": "assistant"}
	if text := partsText(resp.Parts, partText); text != "" {
		msg["content"] = text
	} else {
		msg["content"] = ""
	}
	if reasoning := partsText(resp.Parts, partReasoning); reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	var calls []any
	for _, p := range resp.Parts {
		if p.Kind != partToolUse {
			continue
		}
		id := p.ToolID
		if id == "" {
			id = newProtocolID("call_")
		}
		calls = append(calls, map[string]any{
			"id":   id,
			"type": "function",
			"function": map[string]any{
				"name":      p.ToolName,
				"arguments": toolArguments(p.ToolInput),
			},
		})
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	finish := chatFinishFromStop(resp.StopReason)
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	body := map[string]any{
		"id":      newProtocolID("chatcmpl-"),
		"object":  "chat.completion",
		"created": responseCreated(resp),
		"model":   resp.Model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       msg,
				"finish_reason": finish,
			},
		},
	}
	if resp.Usage != nil {
		body["usage"] = chatUsageJSON(*resp.Usage)
	}
	return json.Marshal(body)
}

func renderResponsesResponse(resp *canonResponse) ([]byte, error) {
	output := make([]any, 0, 3)
	if reasoning := partsText(resp.Parts, partReasoning); reasoning != "" {
		output = append(output, map[string]any{
			"type":   "reasoning",
			"id":     newProtocolID("rs_"),
			"status": "completed",
			"summary": []any{
				map[string]any{"type": "summary_text", "text": reasoning},
			},
		})
	}
	if text := partsText(resp.Parts, partText); text != "" {
		output = append(output, map[string]any{
			"type":   "message",
			"id":     newProtocolID("msg_"),
			"status": "completed",
			"role":   "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
			},
		})
	}
	for _, p := range resp.Parts {
		if p.Kind != partToolUse {
			continue
		}
		callID := p.ToolID
		if callID == "" {
			callID = newProtocolID("call_")
		}
		output = append(output, map[string]any{
			"type":      "function_call",
			"id":        newProtocolID("fc_"),
			"call_id":   callID,
			"name":      p.ToolName,
			"arguments": toolArguments(p.ToolInput),
			"status":    "completed",
		})
	}

	status := "completed"
	var incomplete any
	switch resp.StopReason {
	case stopMaxTokens:
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	case stopRefusal:
		status = "incomplete"
		incomplete = map[string]any{"reason": "content_filter"}
	}
	body := map[string]any{
		"id":                 newProtocolID("resp_"),
		"object":             "response",
		"created_at":         responseCreated(resp),
		"status":             status,
		"model":              resp.Model,
		"output":             output,
		"incomplete_details": incomplete,
	}
	if resp.Usage != nil {
		body["usage"] = responsesUsageJSON(*resp.Usage)
	}
	return json.Marshal(body)
}

func renderAnthropicResponse(resp *canonResponse) ([]byte, error) {
	content := make([]any, 0, 3)
	if text := partsText(resp.Parts, partText); text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	toolCount := 0
	for _, p := range resp.Parts {
		if p.Kind != partToolUse {
			continue
		}
		toolCount++
		id := p.ToolID
		if id == "" {
			id = newProtocolID("toolu_")
		}
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  p.ToolName,
			"input": normalizeToolInput(p.ToolInput),
		})
	}
	stop := anthropicStop(resp.StopReason)
	if toolCount > 0 && stop != stopMaxTokens {
		stop = stopToolUse
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	body := map[string]any{
		"id":            newProtocolID("msg_"),
		"type":          "message",
		"role":          "assistant",
		"model":         resp.Model,
		"content":       content,
		"stop_reason":   stop,
		"stop_sequence": nil,
	}
	if resp.Usage != nil {
		body["usage"] = anthropicUsageJSON(*resp.Usage)
	} else {
		body["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
	}
	return json.Marshal(body)
}

func responseCreated(resp *canonResponse) int64 {
	if resp.Created > 0 {
		return resp.Created
	}
	return time.Now().Unix()
}

func joinStrings(v []string, sep string) string {
	out := ""
	for i, s := range v {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}
