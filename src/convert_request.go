package main

// 请求侧转换：三种入口协议 -> canonRequest -> 三种上游协议。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const defaultAnthropicMaxTokens int64 = 4096

func parseClientRequest(proto Protocol, raw []byte) (*canonRequest, error) {
	switch proto {
	case protoChat:
		return parseChatRequest(raw)
	case protoResponses:
		return parseResponsesRequest(raw)
	case protoAnthropic:
		return parseAnthropicRequest(raw)
	}
	return nil, fmt.Errorf("unsupported protocol %q", proto)
}

func renderRequest(proto Protocol, req *canonRequest) ([]byte, error) {
	switch proto {
	case protoChat:
		return renderChatRequest(req)
	case protoResponses:
		return renderResponsesRequest(req)
	case protoAnthropic:
		return renderAnthropicRequest(req)
	}
	return nil, fmt.Errorf("unsupported protocol %q", proto)
}

// ---------------------------------------------------------------------------
// 公共小工具
// ---------------------------------------------------------------------------

func isJSONEmpty(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// rawText 兼容字符串与 {text/content:...} 两种形态。
func rawText(raw json.RawMessage) string {
	if isJSONEmpty(raw) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Text    string `json:"text"`
		Content string `json:"content"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if obj.Text != "" {
			return obj.Text
		}
		return obj.Content
	}
	return ""
}

func parseStopList(raw json.RawMessage) []string {
	if isJSONEmpty(raw) {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []string{s}
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		out := make([]string, 0, len(arr))
		for _, v := range arr {
			if strings.TrimSpace(v) != "" {
				out = append(out, v)
			}
		}
		return out
	}
	return nil
}

func parseImageURLRaw(raw json.RawMessage) string {
	if isJSONEmpty(raw) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.URL
	}
	return ""
}

// ---------------------------------------------------------------------------
// Chat Completions 入口
// ---------------------------------------------------------------------------

type chatToolCallIn struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatMessageIn struct {
	Role             string            `json:"role"`
	Content          json.RawMessage   `json:"content"`
	Name             string            `json:"name"`
	ToolCallID       string            `json:"tool_call_id"`
	ToolCalls        []chatToolCallIn  `json:"tool_calls"`
	ReasoningContent json.RawMessage   `json:"reasoning_content"`
	Reasoning        json.RawMessage   `json:"reasoning"`
	FunctionCall     *chatFunctionCall `json:"function_call"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatRequestIn struct {
	Model               string          `json:"model"`
	Messages            []chatMessageIn `json:"messages"`
	Stream              bool            `json:"stream"`
	MaxTokens           int64           `json:"max_tokens"`
	MaxCompletionTokens int64           `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	Stop                json.RawMessage `json:"stop"`
	Tools               json.RawMessage `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	N                   int             `json:"n"`
}

func parseChatRequest(raw []byte) (*canonRequest, error) {
	var in chatRequestIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	req := &canonRequest{
		Model:       strings.TrimSpace(in.Model),
		Stream:      in.Stream,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		MaxOutput:   in.MaxTokens,
		Choices:     in.N,
	}
	if req.MaxOutput <= 0 {
		req.MaxOutput = in.MaxCompletionTokens
	}
	req.Stop = parseStopList(in.Stop)
	req.Tools = parseChatTools(in.Tools)
	req.ToolChoice = parseChatToolChoice(in.ToolChoice)

	for _, m := range in.Messages {
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			text := partsText(parseChatContent(m.Content), partText)
			if text != "" {
				if req.System != "" {
					req.System += "\n\n"
				}
				req.System += text
			}
		case "assistant":
			parts := parseChatContent(m.Content)
			for _, tc := range m.ToolCalls {
				parts = append(parts, canonPart{
					Kind:      partToolUse,
					ToolID:    tc.ID,
					ToolName:  tc.Function.Name,
					ToolInput: normalizeToolInput(json.RawMessage(tc.Function.Arguments)),
				})
			}
			if m.FunctionCall != nil {
				parts = append(parts, canonPart{
					Kind:      partToolUse,
					ToolName:  m.FunctionCall.Name,
					ToolInput: normalizeToolInput(json.RawMessage(m.FunctionCall.Arguments)),
				})
			}
			reasoning := rawText(m.ReasoningContent)
			if reasoning == "" {
				reasoning = rawText(m.Reasoning)
			}
			if reasoning != "" {
				parts = append(parts, canonPart{Kind: partReasoning, Text: reasoning})
			}
			req.Messages = append(req.Messages, canonMessage{Role: "assistant", Parts: parts})
		case "tool", "function":
			text := partsText(parseChatContent(m.Content), partText)
			req.Messages = append(req.Messages, canonMessage{
				Role:  "user",
				Parts: []canonPart{{Kind: partToolResult, ToolID: m.ToolCallID, ToolResult: text}},
			})
		default:
			req.Messages = append(req.Messages, canonMessage{Role: "user", Parts: parseChatContent(m.Content)})
		}
	}
	return req, nil
}

func parseChatContent(raw json.RawMessage) []canonPart {
	if isJSONEmpty(raw) {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []canonPart{{Kind: partText, Text: s}}
	}
	var arr []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Refusal  string          `json:"refusal"`
		ImageURL json.RawMessage `json:"image_url"`
	}
	if json.Unmarshal(raw, &arr) == nil {
		var out []canonPart
		for _, p := range arr {
			switch p.Type {
			case "text", "input_text", "output_text":
				if p.Text != "" {
					out = append(out, canonPart{Kind: partText, Text: p.Text})
				}
			case "refusal":
				if p.Refusal != "" {
					out = append(out, canonPart{Kind: partText, Text: p.Refusal})
				}
			case "image_url", "input_image":
				if u := parseImageURLRaw(p.ImageURL); u != "" {
					out = append(out, canonPart{Kind: partImage, ImageURL: u})
				}
			}
		}
		return out
	}
	return []canonPart{{Kind: partText, Text: string(bytes.TrimSpace(raw))}}
}

func parseChatTools(raw json.RawMessage) []canonTool {
	if isJSONEmpty(raw) {
		return nil
	}
	var arr []struct {
		Type     string `json:"type"`
		Function *struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	if json.Unmarshal(raw, &arr) != nil {
		return nil
	}
	var out []canonTool
	for _, t := range arr {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		name, desc, params := t.Name, t.Description, t.Parameters
		if t.Function != nil {
			name, desc, params = t.Function.Name, t.Function.Description, t.Function.Parameters
		}
		if strings.TrimSpace(name) == "" {
			continue
		}
		out = append(out, canonTool{Name: name, Description: desc, Parameters: params})
	}
	return out
}

func parseChatToolChoice(raw json.RawMessage) *canonToolChoice {
	if isJSONEmpty(raw) {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "auto":
			return &canonToolChoice{Mode: "auto"}
		case "none":
			return &canonToolChoice{Mode: "none"}
		case "required", "any":
			return &canonToolChoice{Mode: "required"}
		}
		return nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		name := obj.Function.Name
		if name == "" {
			name = obj.Name
		}
		if name != "" {
			return &canonToolChoice{Mode: "tool", Name: name}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Responses 入口
// ---------------------------------------------------------------------------

type responsesToolIn struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type responsesInputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Output    json.RawMessage `json:"output"`
	Summary   json.RawMessage `json:"summary"`
}

type responsesRequestIn struct {
	Model              string            `json:"model"`
	Input              json.RawMessage   `json:"input"`
	Instructions       json.RawMessage   `json:"instructions"`
	Stream             bool              `json:"stream"`
	MaxOutputTokens    int64             `json:"max_output_tokens"`
	Temperature        *float64          `json:"temperature"`
	TopP               *float64          `json:"top_p"`
	Tools              []responsesToolIn `json:"tools"`
	ToolChoice         json.RawMessage   `json:"tool_choice"`
	PreviousResponseID string            `json:"previous_response_id"`
}

func parseResponsesRequest(raw []byte) (*canonRequest, error) {
	var in responsesRequestIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if strings.TrimSpace(in.PreviousResponseID) != "" {
		return nil, fmt.Errorf("previous_response_id is not supported by this stateless gateway; send the full input instead")
	}
	req := &canonRequest{
		Model:       strings.TrimSpace(in.Model),
		System:      parseResponsesInstructions(in.Instructions),
		Stream:      in.Stream,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		MaxOutput:   in.MaxOutputTokens,
	}
	system, msgs, err := parseResponsesInput(in.Input)
	if err != nil {
		return nil, err
	}
	if system != "" {
		if req.System != "" {
			req.System += "\n\n"
		}
		req.System += system
	}
	req.Messages = msgs
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			continue // 内置工具（web_search 等）无法跨协议转换
		}
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		req.Tools = append(req.Tools, canonTool{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	req.ToolChoice = parseResponsesToolChoice(in.ToolChoice)
	return req, nil
}

func parseResponsesInstructions(raw json.RawMessage) string {
	if isJSONEmpty(raw) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

func parseResponsesInput(raw json.RawMessage) (system string, msgs []canonMessage, err error) {
	if isJSONEmpty(raw) {
		return "", nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return "", nil, nil
		}
		return "", []canonMessage{{Role: "user", Parts: []canonPart{{Kind: partText, Text: s}}}}, nil
	}
	var items []responsesInputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return "", nil, fmt.Errorf("invalid input: %w", err)
	}
	var systemParts []string
	appendMsg := func(role string, parts []canonPart) {
		if len(parts) == 0 {
			return
		}
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Parts = append(msgs[n-1].Parts, parts...)
			return
		}
		msgs = append(msgs, canonMessage{Role: role, Parts: parts})
	}

	for _, it := range items {
		typ := strings.ToLower(strings.TrimSpace(it.Type))
		if typ == "" {
			typ = "message"
		}
		switch typ {
		case "message":
			role := strings.ToLower(strings.TrimSpace(it.Role))
			parts := parseResponsesContent(it.Content)
			switch role {
			case "system", "developer":
				if text := partsText(parts, partText); text != "" {
					systemParts = append(systemParts, text)
				}
			case "assistant":
				appendMsg("assistant", parts)
			default:
				appendMsg("user", parts)
			}
		case "function_call":
			callID := it.CallID
			if callID == "" {
				callID = it.ID
			}
			appendMsg("assistant", []canonPart{{
				Kind:      partToolUse,
				ToolID:    callID,
				ToolName:  it.Name,
				ToolInput: responsesArguments(it.Arguments),
			}})
		case "function_call_output":
			appendMsg("user", []canonPart{{
				Kind:       partToolResult,
				ToolID:     it.CallID,
				ToolResult: responsesOutputText(it.Output),
			}})
		case "reasoning":
			if text := responsesReasoningText(it.Summary, it.Content); text != "" {
				appendMsg("assistant", []canonPart{{Kind: partReasoning, Text: text}})
			}
		}
	}
	return strings.Join(systemParts, "\n\n"), msgs, nil
}

func parseResponsesContent(raw json.RawMessage) []canonPart {
	if isJSONEmpty(raw) {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []canonPart{{Kind: partText, Text: s}}
	}
	var arr []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Refusal  string          `json:"refusal"`
		ImageURL json.RawMessage `json:"image_url"`
	}
	if json.Unmarshal(raw, &arr) != nil {
		return nil
	}
	var out []canonPart
	for _, p := range arr {
		switch p.Type {
		case "input_text", "output_text", "text":
			if p.Text != "" {
				out = append(out, canonPart{Kind: partText, Text: p.Text})
			}
		case "refusal":
			if p.Refusal != "" {
				out = append(out, canonPart{Kind: partText, Text: p.Refusal})
			}
		case "input_image", "image_url":
			if u := parseImageURLRaw(p.ImageURL); u != "" {
				out = append(out, canonPart{Kind: partImage, ImageURL: u})
			}
		}
	}
	return out
}

func responsesOutputText(raw json.RawMessage) string {
	if isJSONEmpty(raw) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var arr []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &arr) == nil {
		var parts []string
		for _, p := range arr {
			if p.Text != "" {
				parts = append(parts, p.Text)
			}
		}
		return strings.Join(parts, "")
	}
	return string(bytes.TrimSpace(raw))
}

func responsesReasoningText(summary, content json.RawMessage) string {
	var out []string
	for _, raw := range []json.RawMessage{summary, content} {
		if isJSONEmpty(raw) {
			continue
		}
		var arr []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &arr) == nil {
			for _, p := range arr {
				if p.Text != "" {
					out = append(out, p.Text)
				}
			}
		}
	}
	return strings.Join(out, "")
}

func parseResponsesToolChoice(raw json.RawMessage) *canonToolChoice {
	if isJSONEmpty(raw) {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "auto":
			return &canonToolChoice{Mode: "auto"}
		case "none":
			return &canonToolChoice{Mode: "none"}
		case "required", "any":
			return &canonToolChoice{Mode: "required"}
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if strings.EqualFold(obj.Type, "function") && obj.Name != "" {
			return &canonToolChoice{Mode: "tool", Name: obj.Name}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Anthropic Messages 入口
// ---------------------------------------------------------------------------

type anthropicToolIn struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicMessageIn struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicRequestIn struct {
	Model         string               `json:"model"`
	System        json.RawMessage      `json:"system"`
	Messages      []anthropicMessageIn `json:"messages"`
	MaxTokens     int64                `json:"max_tokens"`
	Temperature   *float64             `json:"temperature"`
	TopP          *float64             `json:"top_p"`
	StopSequences []string             `json:"stop_sequences"`
	Stream        bool                 `json:"stream"`
	Tools         []anthropicToolIn    `json:"tools"`
	ToolChoice    json.RawMessage      `json:"tool_choice"`
}

func parseAnthropicRequest(raw []byte) (*canonRequest, error) {
	var in anthropicRequestIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	req := &canonRequest{
		Model:       strings.TrimSpace(in.Model),
		System:      parseAnthropicSystem(in.System),
		Stream:      in.Stream,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		MaxOutput:   in.MaxTokens,
		Stop:        in.StopSequences,
	}
	for _, m := range in.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role != "assistant" {
			role = "user"
		}
		req.Messages = append(req.Messages, canonMessage{Role: role, Parts: parseAnthropicContent(m.Content)})
	}
	for _, t := range in.Tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		req.Tools = append(req.Tools, canonTool{Name: t.Name, Description: t.Description, Parameters: t.InputSchema})
	}
	req.ToolChoice = parseAnthropicToolChoice(in.ToolChoice)
	return req, nil
}

func parseAnthropicSystem(raw json.RawMessage) string {
	if isJSONEmpty(raw) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

func parseAnthropicContent(raw json.RawMessage) []canonPart {
	if isJSONEmpty(raw) {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []canonPart{{Kind: partText, Text: s}}
	}
	var blocks []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
		ToolID   string          `json:"tool_use_id"`
		Content  json.RawMessage `json:"content"`
		IsError  bool            `json:"is_error"`
		Source   *struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
			URL       string `json:"url"`
		} `json:"source"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var out []canonPart
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out = append(out, canonPart{Kind: partText, Text: b.Text})
			}
		case "thinking":
			if b.Thinking != "" {
				out = append(out, canonPart{Kind: partReasoning, Text: b.Thinking})
			}
		case "image":
			if b.Source == nil {
				continue
			}
			url := b.Source.URL
			if strings.EqualFold(b.Source.Type, "base64") && b.Source.Data != "" {
				mt := b.Source.MediaType
				if mt == "" {
					mt = "image/png"
				}
				url = "data:" + mt + ";base64," + b.Source.Data
			}
			if url != "" {
				out = append(out, canonPart{Kind: partImage, ImageURL: url})
			}
		case "tool_use":
			out = append(out, canonPart{
				Kind:      partToolUse,
				ToolID:    b.ID,
				ToolName:  b.Name,
				ToolInput: normalizeToolInput(b.Input),
			})
		case "tool_result":
			out = append(out, canonPart{
				Kind:       partToolResult,
				ToolID:     b.ToolID,
				ToolResult: anthropicContentText(b.Content),
				IsError:    b.IsError,
			})
		}
	}
	return out
}

func anthropicContentText(raw json.RawMessage) string {
	if isJSONEmpty(raw) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func parseAnthropicToolChoice(raw json.RawMessage) *canonToolChoice {
	if isJSONEmpty(raw) {
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(obj.Type)) {
	case "auto":
		return &canonToolChoice{Mode: "auto"}
	case "any":
		return &canonToolChoice{Mode: "required"}
	case "none":
		return &canonToolChoice{Mode: "none"}
	case "tool":
		if obj.Name != "" {
			return &canonToolChoice{Mode: "tool", Name: obj.Name}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 渲染：canonRequest -> 上游请求
// ---------------------------------------------------------------------------

func renderChatRequest(req *canonRequest) ([]byte, error) {
	body := map[string]any{
		"model":    req.Model,
		"messages": chatMessages(req),
		"stream":   req.Stream,
	}
	if req.MaxOutput > 0 {
		body["max_tokens"] = req.MaxOutput
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if len(req.Stop) == 1 {
		body["stop"] = req.Stop[0]
	} else if len(req.Stop) > 1 {
		body["stop"] = req.Stop
	}
	if len(req.Tools) > 0 {
		body["tools"] = chatTools(req.Tools)
	}
	if req.ToolChoice != nil {
		body["tool_choice"] = chatToolChoice(req.ToolChoice)
	}
	if req.Stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(body)
}

func chatMessages(req *canonRequest) []any {
	out := make([]any, 0, len(req.Messages)+1)
	if req.System != "" {
		out = append(out, map[string]any{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			var texts []string
			var calls []any
			for _, p := range m.Parts {
				switch p.Kind {
				case partText:
					if p.Text != "" {
						texts = append(texts, p.Text)
					}
				case partToolUse:
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
			}
			if len(texts) == 0 && len(calls) == 0 {
				continue
			}
			msg := map[string]any{"role": "assistant"}
			if text := strings.Join(texts, ""); text != "" {
				msg["content"] = text
			} else {
				msg["content"] = nil
			}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			out = append(out, msg)
			continue
		}

		// user：Chat 要求 tool 结果单独成消息，且紧跟在 assistant tool_calls 之后。
		for _, p := range m.Parts {
			if p.Kind == partToolResult {
				out = append(out, map[string]any{
					"role":         "tool",
					"tool_call_id": p.ToolID,
					"content":      p.ToolResult,
				})
			}
		}
		var texts []string
		var rich []any
		for _, p := range m.Parts {
			switch p.Kind {
			case partText:
				if p.Text != "" {
					texts = append(texts, p.Text)
					rich = append(rich, map[string]any{"type": "text", "text": p.Text})
				}
			case partImage:
				rich = append(rich, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": p.ImageURL},
				})
			}
		}
		if len(rich) == 0 {
			continue
		}
		if len(texts) == len(rich) {
			out = append(out, map[string]any{"role": "user", "content": strings.Join(texts, "")})
		} else {
			out = append(out, map[string]any{"role": "user", "content": rich})
		}
	}
	return out
}

func chatTools(tools []canonTool) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		params := normalizeToolInput(t.Parameters)
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  params,
			},
		})
	}
	return out
}

func chatToolChoice(choice *canonToolChoice) any {
	switch choice.Mode {
	case "none":
		return "none"
	case "required":
		return "required"
	case "tool":
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": choice.Name,
			},
		}
	default:
		return "auto"
	}
}

func renderResponsesRequest(req *canonRequest) ([]byte, error) {
	items := responsesInputItems(req)
	var input any = items
	if len(items) == 0 {
		input = ""
	}
	body := map[string]any{
		"model":  req.Model,
		"input":  input,
		"stream": req.Stream,
	}
	if req.System != "" {
		body["instructions"] = req.System
	}
	if req.MaxOutput > 0 {
		body["max_output_tokens"] = req.MaxOutput
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type":        "function",
				"name":        t.Name,
				"description": t.Description,
				"parameters":  normalizeToolInput(t.Parameters),
			})
		}
		body["tools"] = tools
	}
	if req.ToolChoice != nil {
		body["tool_choice"] = responsesToolChoice(req.ToolChoice)
	}
	return json.Marshal(body)
}

func responsesInputItems(req *canonRequest) []any {
	items := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		}
		var pending []any
		flush := func() {
			if len(pending) == 0 {
				return
			}
			items = append(items, map[string]any{
				"type":    "message",
				"role":    role,
				"content": pending,
			})
			pending = nil
		}
		for _, p := range m.Parts {
			switch p.Kind {
			case partText:
				if p.Text != "" {
					pending = append(pending, map[string]any{"type": "input_text", "text": p.Text})
				}
			case partImage:
				if role == "user" && p.ImageURL != "" {
					pending = append(pending, map[string]any{"type": "input_image", "image_url": p.ImageURL})
				}
			case partToolUse:
				if role != "assistant" {
					continue
				}
				flush()
				id := p.ToolID
				if id == "" {
					id = newProtocolID("call_")
				}
				items = append(items, map[string]any{
					"type":      "function_call",
					"call_id":   id,
					"name":      p.ToolName,
					"arguments": toolArguments(p.ToolInput),
				})
			case partToolResult:
				if role != "user" {
					continue
				}
				flush()
				items = append(items, map[string]any{
					"type":    "function_call_output",
					"call_id": p.ToolID,
					"output":  p.ToolResult,
				})
			}
		}
		flush()
	}
	return items
}

func responsesToolChoice(choice *canonToolChoice) any {
	switch choice.Mode {
	case "none":
		return "none"
	case "required":
		return "required"
	case "tool":
		return map[string]any{"type": "function", "name": choice.Name}
	default:
		return "auto"
	}
}

func renderAnthropicRequest(req *canonRequest) ([]byte, error) {
	maxTokens := req.MaxOutput
	if maxTokens <= 0 {
		maxTokens = defaultAnthropicMaxTokens
	}
	body := map[string]any{
		"model":      req.Model,
		"max_tokens": maxTokens,
		"messages":   anthropicMessages(req),
		"stream":     req.Stream,
	}
	if req.System != "" {
		body["system"] = req.System
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		body["stop_sequences"] = req.Stop
	}
	// Anthropic 没有 tool_choice=none，禁用工具的方式是不传 tools。
	if len(req.Tools) > 0 && (req.ToolChoice == nil || req.ToolChoice.Mode != "none") {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": normalizeToolInput(t.Parameters),
			})
		}
		body["tools"] = tools
		if req.ToolChoice != nil {
			body["tool_choice"] = anthropicToolChoice(req.ToolChoice)
		}
	}
	return json.Marshal(body)
}

func anthropicMessages(req *canonRequest) []any {
	var out []any
	for _, m := range req.Messages {
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		}
		var blocks []any
		var toolResults []any
		for _, p := range m.Parts {
			switch p.Kind {
			case partText:
				if p.Text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
				}
			case partImage:
				if role != "user" || p.ImageURL == "" {
					continue
				}
				if mt, data, ok := parseImageDataURI(p.ImageURL); ok {
					blocks = append(blocks, map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": mt,
							"data":       data,
						},
					})
				} else {
					blocks = append(blocks, map[string]any{
						"type": "image",
						"source": map[string]any{
							"type": "url",
							"url":  p.ImageURL,
						},
					})
				}
			case partToolUse:
				if role != "assistant" {
					continue
				}
				id := p.ToolID
				if id == "" {
					id = newProtocolID("toolu_")
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    id,
					"name":  p.ToolName,
					"input": normalizeToolInput(p.ToolInput),
				})
			case partToolResult:
				if role != "user" {
					continue
				}
				block := map[string]any{
					"type":        "tool_result",
					"tool_use_id": p.ToolID,
					"content":     p.ToolResult,
				}
				if p.IsError {
					block["is_error"] = true
				}
				toolResults = append(toolResults, block)
			}
		}
		// Anthropic 要求 tool_result 位于 user 消息的前部。
		if role == "user" && len(toolResults) > 0 {
			blocks = append(toolResults, blocks...)
		}
		if len(blocks) == 0 {
			continue
		}
		if n := len(out); n > 0 {
			last := out[n-1].(map[string]any)
			if last["role"] == role {
				last["content"] = append(last["content"].([]any), blocks...)
				continue
			}
		}
		out = append(out, map[string]any{"role": role, "content": blocks})
	}
	return out
}

func anthropicToolChoice(choice *canonToolChoice) any {
	switch choice.Mode {
	case "required":
		return map[string]any{"type": "any"}
	case "tool":
		return map[string]any{"type": "tool", "name": choice.Name}
	default:
		return map[string]any{"type": "auto"}
	}
}

// maxOutputForProtocol 在跨协议预估时补齐 Anthropic 必填的 max_tokens，
// 避免预扣金额明显偏低。
func maxOutputForProtocol(req *canonRequest, upProto Protocol) int64 {
	if upProto == protoAnthropic && req.MaxOutput <= 0 {
		return defaultAnthropicMaxTokens
	}
	return req.MaxOutput
}
