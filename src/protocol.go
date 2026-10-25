package main

// 协议层：Chat Completions / Responses / Anthropic Messages 的统一内部表示。
//
// 网关把三种协议的请求都解析成 canonRequest，把上游响应都解析成
// canonResponse / canonEvent，再按调用方协议重新编码。这样任意入口
// 都可以配任意协议的上游，转换只需要维护 3 个解析器 + 3 个渲染器。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

type Protocol string

const (
	protoChat      Protocol = "chat"
	protoResponses Protocol = "responses"
	protoAnthropic Protocol = "anthropic"
)

// normalizeProtocol 接受配置里的别名，未知值原样返回交给校验处理。
func normalizeProtocol(raw string) Protocol {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "chat", "openai", "openai-chat", "chat-completions", "chat_completions":
		return protoChat
	case "responses", "response", "openai-responses", "openai_responses":
		return protoResponses
	case "anthropic", "messages", "anthropic-messages", "anthropic_messages", "claude":
		return protoAnthropic
	}
	return Protocol(strings.ToLower(strings.TrimSpace(raw)))
}

func protocolValid(p Protocol) bool {
	switch p {
	case protoChat, protoResponses, protoAnthropic:
		return true
	}
	return false
}

// requestProtocol 用于鉴权失败/页面请求时决定错误体格式。
func requestProtocol(r *http.Request) Protocol {
	switch {
	case r.URL.Path == "/v1/responses" || strings.HasPrefix(r.URL.Path, "/v1/responses/"):
		return protoResponses
	case r.URL.Path == "/v1/messages" || strings.HasPrefix(r.URL.Path, "/v1/messages/"):
		return protoAnthropic
	case r.URL.Path == "/v1/models" &&
		(strings.TrimSpace(r.Header.Get("anthropic-version")) != "" || strings.TrimSpace(r.Header.Get("x-api-key")) != ""):
		return protoAnthropic
	}
	return protoChat
}

// ---------------------------------------------------------------------------
// 统一内部表示
// ---------------------------------------------------------------------------

const (
	partText       = "text"
	partImage      = "image"
	partToolUse    = "tool_use"
	partToolResult = "tool_result"
	partReasoning  = "reasoning"
)

const (
	stopEndTurn      = "end_turn"
	stopMaxTokens    = "max_tokens"
	stopToolUse      = "tool_use"
	stopStopSequence = "stop_sequence"
	stopRefusal      = "refusal"
)

type canonPart struct {
	Kind       string
	Text       string
	ImageURL   string // http(s) URL 或 data URI
	ToolID     string
	ToolName   string
	ToolInput  json.RawMessage // tool_use 的入参（JSON 对象）
	ToolResult string
	IsError    bool
}

type canonMessage struct {
	Role  string // user | assistant
	Parts []canonPart
}

type canonTool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

type canonToolChoice struct {
	Mode string // auto | none | required | tool
	Name string
}

type canonRequest struct {
	Model       string
	System      string
	Messages    []canonMessage
	MaxOutput   int64
	Temperature *float64
	TopP        *float64
	Stop        []string
	Stream      bool
	Tools       []canonTool
	ToolChoice  *canonToolChoice
	Choices     int // Chat 的 n 参数；跨协议只支持 1
}

type canonResponse struct {
	ID         string
	Model      string
	Created    int64
	Parts      []canonPart
	StopReason string
	Usage      *canonUsage
}

// canonUsage 中 Input 是含缓存命中的全部输入 token。
type canonUsage struct {
	Input     int64
	Output    int64
	Cached    int64
	Reasoning int64
}

func (u canonUsage) total() int64 { return u.Input + u.Output }

func (u canonUsage) empty() bool {
	return u.Input == 0 && u.Output == 0 && u.Cached == 0 && u.Reasoning == 0
}

func (u canonUsage) toUsage() *Usage {
	if u.empty() {
		return nil
	}
	out := &Usage{
		PromptTokens:         u.Input,
		CompletionTokens:     u.Output,
		TotalTokens:          u.Input + u.Output,
		PromptCacheHitTokens: u.Cached,
	}
	if u.Cached > 0 {
		out.PromptTokensDetails = &promptTokensDetails{CachedTokens: u.Cached}
	}
	if u.Reasoning > 0 {
		out.CompletionTokensDetails = &completionTokensDetails{ReasoningTokens: u.Reasoning}
	}
	return out
}

// canonEvent 是流式转换的中间事件。
type canonEvent struct {
	Kind      string // text | reasoning | tool_start | tool_args | finish
	Text      string
	ToolIndex int
	ToolID    string
	ToolName  string
	Stop      string
	Usage     *canonUsage
	Err       string
}

// estimate 与原有 Chat 估算保持同一量纲；图片按少量固定 token 粗估。
func (r *canonRequest) estimate() tokenEstimate {
	var prompt int64 = 3
	if r.System != "" {
		prompt += messageOverhead + countText(r.System)
	}
	for _, m := range r.Messages {
		prompt += messageOverhead
		for _, p := range m.Parts {
			switch p.Kind {
			case partText:
				prompt += countText(p.Text)
			case partImage:
				prompt += 85
			case partToolUse:
				prompt += countText(p.ToolName) + countText(string(p.ToolInput))
			case partToolResult:
				prompt += countText(p.ToolResult)
			}
		}
	}
	for _, t := range r.Tools {
		prompt += messageOverhead + countText(t.Name) + countText(t.Description) + countText(string(t.Parameters))
	}
	completion := r.MaxOutput
	if completion <= 0 {
		completion = prompt
		if completion < minCompletionReserve {
			completion = minCompletionReserve
		}
	}
	return tokenEstimate{Prompt: prompt, Completion: completion}
}

func partsText(parts []canonPart, kind string) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Kind == kind {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func hasPart(parts []canonPart, kind string) bool {
	for _, p := range parts {
		if p.Kind == kind {
			return true
		}
	}
	return false
}

// normalizeToolInput 保证 tool_use 的 input 一定是 JSON 对象。
func normalizeToolInput(raw json.RawMessage) json.RawMessage {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) || !json.Valid(t) {
		return json.RawMessage(`{}`)
	}
	return t
}

func toolArguments(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return "{}"
	}
	return string(t)
}

func newProtocolID(prefix string) string { return prefix + newRequestID() }

func responseNow() int64 { return time.Now().Unix() }

func canonUsageToUsage(u *canonUsage) *Usage {
	if u == nil {
		return nil
	}
	return u.toUsage()
}

// parseImageDataURI 解析 data:image/...;base64,...。
func parseImageDataURI(s string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(s, "data:") {
		return "", "", false
	}
	meta, payload, found := strings.Cut(strings.TrimPrefix(s, "data:"), ",")
	if !found {
		return "", "", false
	}
	fields := strings.Split(meta, ";")
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "image/") {
		return "", "", false
	}
	isBase64 := false
	for _, f := range fields[1:] {
		if strings.EqualFold(strings.TrimSpace(f), "base64") {
			isBase64 = true
		}
	}
	if !isBase64 || payload == "" {
		return "", "", false
	}
	return fields[0], payload, true
}

// ---------------------------------------------------------------------------
// Usage 映射
// ---------------------------------------------------------------------------

func usageFromChat(u *Usage) (canonUsage, bool) {
	if u == nil {
		return canonUsage{}, false
	}
	in := u.PromptTokens
	if in == 0 && u.TotalTokens > 0 && u.CompletionTokens <= u.TotalTokens {
		in = u.TotalTokens - u.CompletionTokens
	}
	out := canonUsage{Input: in, Output: u.CompletionTokens, Cached: u.cacheHitTokens()}
	if u.CompletionTokensDetails != nil {
		out.Reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	return out, true
}

// anthropicUsage 是 Anthropic 的 usage 结构。input_tokens 不含缓存。
type anthropicUsage struct {
	InputTokens          int64 `json:"input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens  int64 `json:"cache_creation_input_tokens"`
}

func usageFromAnthropic(u *anthropicUsage) (canonUsage, bool) {
	if u == nil {
		return canonUsage{}, false
	}
	return canonUsage{
		Input:  u.InputTokens + u.CacheReadInputTokens + u.CacheCreationTokens,
		Output: u.OutputTokens,
		Cached: u.CacheReadInputTokens,
	}, true
}

// responsesUsage 是 Responses API 的 usage 结构。
type responsesUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	TotalTokens        int64 `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func usageFromResponses(u *responsesUsage) (canonUsage, bool) {
	if u == nil {
		return canonUsage{}, false
	}
	out := canonUsage{Input: u.InputTokens, Output: u.OutputTokens}
	if u.InputTokensDetails != nil {
		out.Cached = u.InputTokensDetails.CachedTokens
	}
	if u.OutputTokensDetails != nil {
		out.Reasoning = u.OutputTokensDetails.ReasoningTokens
	}
	return out, true
}

// chatUsageJSON 把统一用量渲染成 Chat Completions 的 usage。
func chatUsageJSON(u canonUsage) map[string]any {
	out := map[string]any{
		"prompt_tokens":     u.Input,
		"completion_tokens": u.Output,
		"total_tokens":      u.Input + u.Output,
	}
	if u.Cached > 0 {
		out["prompt_cache_hit_tokens"] = u.Cached
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": u.Cached}
	}
	if u.Reasoning > 0 {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": u.Reasoning}
	}
	return out
}

func anthropicUsageJSON(u canonUsage) map[string]any {
	input := u.Input - u.Cached
	if input < 0 {
		input = 0
	}
	out := map[string]any{"input_tokens": input, "output_tokens": u.Output}
	if u.Cached > 0 {
		out["cache_read_input_tokens"] = u.Cached
	}
	return out
}

func responsesUsageJSON(u canonUsage) map[string]any {
	out := map[string]any{
		"input_tokens":  u.Input,
		"output_tokens": u.Output,
		"total_tokens":  u.Input + u.Output,
		"input_tokens_details": map[string]any{
			"cached_tokens": u.Cached,
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": u.Reasoning,
		},
	}
	return out
}

// ---------------------------------------------------------------------------
// SSE 读写
// ---------------------------------------------------------------------------

type sseEvent struct {
	Name string
	Data string
}

func readSSE(r io.Reader, fn func(sseEvent) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var name string
	var data []string
	flush := func() error {
		if name == "" && len(data) == 0 {
			return nil
		}
		ev := sseEvent{Name: name, Data: strings.Join(data, "\n")}
		name, data = "", nil
		return fn(ev)
	}
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			field, value = line, ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			name = strings.TrimSpace(value)
		case "data":
			data = append(data, value)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush()
}

type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	proto   Protocol
	seq     int
	err     error
}

func newSSEWriter(w http.ResponseWriter, proto Protocol) (*sseWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming unsupported by server")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &sseWriter{w: w, flusher: flusher, proto: proto}, nil
}

func (s *sseWriter) event(name string, payload any) {
	if s.err != nil {
		return
	}
	if s.proto == protoResponses {
		if m, ok := payload.(map[string]any); ok {
			if _, exists := m["sequence_number"]; !exists {
				m["sequence_number"] = s.seq
				s.seq++
			}
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		s.err = err
		return
	}
	var b strings.Builder
	if s.proto != protoChat && name != "" {
		b.WriteString("event: ")
		b.WriteString(name)
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	b.Write(body)
	b.WriteString("\n\n")
	if _, err := io.WriteString(s.w, b.String()); err != nil {
		s.err = err
		return
	}
	s.flusher.Flush()
}

func (s *sseWriter) raw(chunk string) {
	if s.err != nil {
		return
	}
	if _, err := io.WriteString(s.w, chunk); err != nil {
		s.err = err
		return
	}
	s.flusher.Flush()
}

// ---------------------------------------------------------------------------
// 错误格式与错误体提取
// ---------------------------------------------------------------------------

func writeProtocolError(w http.ResponseWriter, proto Protocol, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if proto == protoAnthropic {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    anthropicErrorType(status),
				"message": message,
			},
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    code,
			"code":    code,
		},
	})
}

func anthropicErrorType(status int) string {
	switch {
	case status == http.StatusBadRequest:
		return "invalid_request_error"
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == 529:
		return "overloaded_error"
	case status >= 500:
		return "api_error"
	}
	return "invalid_request_error"
}

func openAIErrorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		return "api_error"
	default:
		if status >= 500 {
			return "api_error"
		}
		return "invalid_request_error"
	}
}

// extractUpstreamError 尽力从各协议错误体里取 message。
func extractUpstreamError(body []byte) string {
	var doc struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &doc); err == nil {
		if doc.Error.Message != "" {
			return doc.Error.Message
		}
		if doc.Message != "" {
			return doc.Message
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 500 {
		s = s[:500] + "…"
	}
	if s == "" {
		return "upstream request failed"
	}
	return s
}

// ---------------------------------------------------------------------------
// 通用小工具
// ---------------------------------------------------------------------------

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func mapChatFinish(finish string) string {
	switch strings.ToLower(strings.TrimSpace(finish)) {
	case "length":
		return stopMaxTokens
	case "tool_calls", "function_call":
		return stopToolUse
	case "content_filter":
		return stopRefusal
	case "stop", "":
		return stopEndTurn
	default:
		return stopEndTurn
	}
}

func chatFinishFromStop(stop string) string {
	switch stop {
	case stopMaxTokens:
		return "length"
	case stopToolUse:
		return "tool_calls"
	case stopRefusal:
		return "content_filter"
	case stopStopSequence, stopEndTurn:
		return "stop"
	default:
		return "stop"
	}
}

func anthropicStop(stop string) string {
	switch stop {
	case stopMaxTokens, stopToolUse, stopStopSequence, stopRefusal, stopEndTurn:
		return stop
	}
	return stopEndTurn
}
