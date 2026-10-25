package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T", v)
	}
	return m
}

func asSlice(t *testing.T, v any) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("expected array, got %T", v)
	}
	return s
}

func getStr(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	s, ok := m[key].(string)
	if !ok {
		t.Fatalf("expected string at %q, got %T", key, m[key])
	}
	return s
}

func getNum(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	n, ok := m[key].(float64)
	if !ok {
		t.Fatalf("expected number at %q, got %T", key, m[key])
	}
	return n
}

func decodeObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, raw)
	}
	return asMap(t, v)
}

func mustParse(t *testing.T, proto Protocol, raw string) *canonRequest {
	t.Helper()
	req, err := parseClientRequest(proto, []byte(raw))
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	return req
}

func mustRenderRequest(t *testing.T, proto Protocol, req *canonRequest) map[string]any {
	t.Helper()
	out, err := renderRequest(proto, req)
	if err != nil {
		t.Fatalf("render request: %v", err)
	}
	return decodeObject(t, out)
}

func TestChatRequestToAnthropic(t *testing.T) {
	req := mustParse(t, protoChat, `{
	  "model":"deepseek-chat","max_tokens":256,"temperature":0.2,"stop":["END"],
	  "messages":[
	    {"role":"system","content":"be brief"},
	    {"role":"user","content":"hi"},
	    {"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},
	    {"role":"tool","tool_call_id":"call_1","content":"sunny"}
	  ],
	  "tools":[{"type":"function","function":{"name":"get_weather","description":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]
	}`)
	body := mustRenderRequest(t, protoAnthropic, req)

	if got := getStr(t, body, "system"); got != "be brief" {
		t.Fatalf("system = %q", got)
	}
	if got := getNum(t, body, "max_tokens"); got != 256 {
		t.Fatalf("max_tokens = %v", got)
	}
	msgs := asSlice(t, body["messages"])
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d", len(msgs))
	}
	assistant := asMap(t, msgs[1])
	if getStr(t, assistant, "role") != "assistant" {
		t.Fatalf("assistant role wrong")
	}
	blocks := asSlice(t, assistant["content"])
	tool := asMap(t, blocks[0])
	if getStr(t, tool, "type") != "tool_use" || getStr(t, tool, "name") != "get_weather" {
		t.Fatalf("tool_use block = %#v", tool)
	}
	if input := asMap(t, tool["input"]); input["city"] != "Paris" {
		t.Fatalf("tool input = %#v", input)
	}
	last := asMap(t, msgs[2])
	result := asMap(t, asSlice(t, last["content"])[0])
	if getStr(t, result, "type") != "tool_result" || getStr(t, result, "tool_use_id") != "call_1" {
		t.Fatalf("tool_result block = %#v", result)
	}
	tools := asSlice(t, body["tools"])
	if schema := asMap(t, asMap(t, tools[0])["input_schema"]); getStr(t, schema, "type") != "object" {
		t.Fatalf("input_schema = %#v", schema)
	}
	stops := asSlice(t, body["stop_sequences"])
	if len(stops) != 1 || stops[0] != "END" {
		t.Fatalf("stop_sequences = %#v", stops)
	}
}

func TestAnthropicRequestToChat(t *testing.T) {
	req := mustParse(t, protoAnthropic, `{
	  "model":"claude-x","max_tokens":128,
	  "system":[{"type":"text","text":"sys1"},{"type":"text","text":"sys2"}],
	  "messages":[
	    {"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]},
	    {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"f","input":{"x":1}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
	  ],
	  "tools":[{"name":"f","description":"d","input_schema":{"type":"object"}}],
	  "tool_choice":{"type":"any"}
	}`)
	body := mustRenderRequest(t, protoChat, req)

	if got := getNum(t, body, "max_tokens"); got != 128 {
		t.Fatalf("max_tokens = %v", got)
	}
	if got := getStr(t, body, "tool_choice"); got != "required" {
		t.Fatalf("tool_choice = %q", got)
	}
	msgs := asSlice(t, body["messages"])
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d: %#v", len(msgs), msgs)
	}
	if getStr(t, asMap(t, msgs[0]), "content") != "sys1\n\nsys2" {
		t.Fatalf("system message = %#v", msgs[0])
	}
	userContent := asSlice(t, asMap(t, msgs[1])["content"])
	if len(userContent) != 2 {
		t.Fatalf("user content = %#v", userContent)
	}
	img := asMap(t, asMap(t, userContent[1])["image_url"])
	if getStr(t, img, "url") != "data:image/png;base64,AAA" {
		t.Fatalf("image url = %#v", img)
	}
	calls := asSlice(t, asMap(t, msgs[2])["tool_calls"])
	fn := asMap(t, asMap(t, calls[0])["function"])
	if getStr(t, fn, "arguments") != `{"x":1}` {
		t.Fatalf("arguments = %q", getStr(t, fn, "arguments"))
	}
	if getStr(t, asMap(t, msgs[3]), "role") != "tool" {
		t.Fatalf("tool message = %#v", msgs[3])
	}
}

func TestResponsesRequestToChat(t *testing.T) {
	req := mustParse(t, protoResponses, `{
	  "model":"m","instructions":"sys","max_output_tokens":64,
	  "input":[
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
	    {"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}"},
	    {"type":"function_call_output","call_id":"call_1","output":"sunny"}
	  ],
	  "tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}],
	  "tool_choice":"auto"
	}`)
	body := mustRenderRequest(t, protoChat, req)

	if got := getNum(t, body, "max_tokens"); got != 64 {
		t.Fatalf("max_tokens = %v", got)
	}
	msgs := asSlice(t, body["messages"])
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d: %#v", len(msgs), msgs)
	}
	calls := asSlice(t, asMap(t, msgs[2])["tool_calls"])
	fn := asMap(t, asMap(t, calls[0])["function"])
	if got := getStr(t, fn, "arguments"); got != `{"city":"Paris"}` {
		t.Fatalf("arguments must be unwrapped, got %q", got)
	}
	if getStr(t, asMap(t, msgs[3]), "content") != "sunny" {
		t.Fatalf("tool output = %#v", msgs[3])
	}
}

func TestChatRequestToResponses(t *testing.T) {
	req := mustParse(t, protoChat, `{
	  "model":"m","max_tokens":32,
	  "messages":[
	    {"role":"user","content":"weather?"},
	    {"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},
	    {"role":"tool","tool_call_id":"call_1","content":"sunny"}
	  ],
	  "tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]
	}`)
	body := mustRenderRequest(t, protoResponses, req)

	if got := getNum(t, body, "max_output_tokens"); got != 32 {
		t.Fatalf("max_output_tokens = %v", got)
	}
	items := asSlice(t, body["input"])
	if len(items) != 3 {
		t.Fatalf("input items = %#v", items)
	}
	call := asMap(t, items[1])
	if getStr(t, call, "type") != "function_call" || getStr(t, call, "arguments") != `{"city":"Paris"}` {
		t.Fatalf("function_call item = %#v", call)
	}
	output := asMap(t, items[2])
	if getStr(t, output, "type") != "function_call_output" || getStr(t, output, "call_id") != "call_1" {
		t.Fatalf("function_call_output = %#v", output)
	}
}

func TestChatResponseToAnthropic(t *testing.T) {
	resp, err := parseCanonicalResponse(protoChat, []byte(`{
	  "id":"chatcmpl-1","model":"m",
	  "choices":[{"message":{
	    "role":"assistant","content":"hello",
	    "reasoning_content":"think",
	    "tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"x\":1}"}}]
	  },"finish_reason":"tool_calls"}],
	  "usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := renderCanonicalResponse(protoAnthropic, resp)
	if err != nil {
		t.Fatal(err)
	}
	body := decodeObject(t, out)
	if getStr(t, body, "stop_reason") != "tool_use" {
		t.Fatalf("stop_reason = %v", body["stop_reason"])
	}
	content := asSlice(t, body["content"])
	if len(content) != 2 {
		t.Fatalf("content = %#v", content)
	}
	usage := asMap(t, body["usage"])
	if getNum(t, usage, "input_tokens") != 6 || getNum(t, usage, "output_tokens") != 5 || getNum(t, usage, "cache_read_input_tokens") != 4 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestAnthropicResponseToChat(t *testing.T) {
	resp, err := parseCanonicalResponse(protoAnthropic, []byte(`{
	  "id":"msg_1","model":"m",
	  "content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}],
	  "stop_reason":"tool_use",
	  "usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":2,"cache_creation_input_tokens":1}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := renderCanonicalResponse(protoChat, resp)
	if err != nil {
		t.Fatal(err)
	}
	body := decodeObject(t, out)
	choice := asMap(t, asSlice(t, body["choices"])[0])
	if getStr(t, choice, "finish_reason") != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	usage := asMap(t, body["usage"])
	if getNum(t, usage, "prompt_tokens") != 13 || getNum(t, usage, "total_tokens") != 18 {
		t.Fatalf("usage = %#v", usage)
	}
	details := asMap(t, usage["prompt_tokens_details"])
	if getNum(t, details, "cached_tokens") != 2 {
		t.Fatalf("cached = %#v", details)
	}
}

func TestResponsesResponseToChat(t *testing.T) {
	resp, err := parseCanonicalResponse(protoResponses, []byte(`{
	  "id":"resp_1","model":"m","status":"completed",
	  "output":[
	    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]},
	    {"type":"function_call","call_id":"call_1","name":"f","arguments":"{\"a\":1}"}
	  ],
	  "usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10,
	    "input_tokens_details":{"cached_tokens":2},
	    "output_tokens_details":{"reasoning_tokens":1}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := renderCanonicalResponse(protoChat, resp)
	if err != nil {
		t.Fatal(err)
	}
	body := decodeObject(t, out)
	usage := asMap(t, body["usage"])
	if getNum(t, usage, "prompt_tokens") != 7 || getNum(t, usage, "completion_tokens") != 3 {
		t.Fatalf("usage = %#v", usage)
	}
	if getNum(t, asMap(t, usage["completion_tokens_details"]), "reasoning_tokens") != 1 {
		t.Fatalf("reasoning details = %#v", usage)
	}
	choice := asMap(t, asSlice(t, body["choices"])[0])
	msg := asMap(t, choice["message"])
	if got := getStr(t, msg, "content"); got != "hi" {
		t.Fatalf("content = %q", got)
	}
}

func TestStreamChatToAnthropic(t *testing.T) {
	out := driveStream(t, protoChat, protoAnthropic, 9, []string{
		`{"id":"c1","choices":[{"delta":{"role":"assistant","content":"He"}}]}`,
		`{"id":"c1","choices":[{"delta":{"content":"llo"}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":""}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\""}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"c1","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`,
		`[DONE]`,
	})
	for _, want := range []string{
		"message_start", `"input_tokens":9`,
		`"text_delta"`, `"text":"He"`, `"text":"llo"`,
		`"tool_use"`, `"input_json_delta"`,
		`"stop_reason":"tool_use"`, `"output_tokens":4`, "message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestStreamAnthropicToChat(t *testing.T) {
	out := driveStream(t, protoAnthropic, protoChat, 0, []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":9,"output_tokens":0,"cache_read_input_tokens":2}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"f","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	})
	for _, want := range []string{
		"chat.completion.chunk", `"content":"Hi"`, `"tool_calls"`, `"arguments":"{\"a\":1}"`,
		`"finish_reason":"tool_calls"`, `"prompt_tokens":11`, `"completion_tokens":3`, "data: [DONE]",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestStreamResponsesToChat(t *testing.T) {
	out := driveStream(t, protoResponses, protoChat, 0, []string{
		`{"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"f"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"a\":1}"}`,
		`{"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"f","arguments":"{\"a\":1}"}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
	})
	for _, want := range []string{
		`"tool_calls"`, `"arguments":"{\"a\":1}"`, `"finish_reason":"tool_calls"`,
		`"prompt_tokens":5`, `"completion_tokens":2`, "data: [DONE]",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestStreamChatToResponses(t *testing.T) {
	out := driveStream(t, protoChat, protoResponses, 0, []string{
		`{"id":"c1","choices":[{"delta":{"role":"assistant","content":"Hi"}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{\"a\":1}"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"c1","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`,
		`[DONE]`,
	})
	for _, want := range []string{
		"response.created", "response.output_item.added", "response.output_text.delta",
		"response.function_call_arguments.delta", "response.completed",
		`"input_tokens":4`, `"output_tokens":2`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestPassthroughUsageCapture(t *testing.T) {
	t.Run("chat", func(t *testing.T) {
		c := newPassthroughUsageCapture(protoChat)
		c.feed([]byte(`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
		u := c.result()
		if u == nil || u.PromptTokens != 3 || u.CompletionTokens != 2 {
			t.Fatalf("usage = %#v", u)
		}
	})
	t.Run("anthropic", func(t *testing.T) {
		c := newPassthroughUsageCapture(protoAnthropic)
		c.feed([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":5,"output_tokens":0,"cache_read_input_tokens":2}}}`))
		c.feed([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`))
		u := c.result()
		if u == nil || u.PromptTokens != 7 || u.CompletionTokens != 3 || u.cacheHitTokens() != 2 {
			t.Fatalf("usage = %#v", u)
		}
	})
	t.Run("responses", func(t *testing.T) {
		c := newPassthroughUsageCapture(protoResponses)
		c.feed([]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":6,"output_tokens":1,"total_tokens":7}}}`))
		u := c.result()
		if u == nil || u.PromptTokens != 6 || u.CompletionTokens != 1 {
			t.Fatalf("usage = %#v", u)
		}
	})
}

func TestImageRoundTrip(t *testing.T) {
	req := mustParse(t, protoChat, `{"model":"m","messages":[{"role":"user","content":[
	  {"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}},
	  {"type":"text","text":"what"}
	]}]}`)
	body := mustRenderRequest(t, protoAnthropic, req)
	user := asMap(t, asSlice(t, body["messages"])[0])
	img := asMap(t, asSlice(t, user["content"])[0])
	src := asMap(t, img["source"])
	if getStr(t, src, "type") != "base64" || getStr(t, src, "data") != "AAA" || getStr(t, src, "media_type") != "image/png" {
		t.Fatalf("image source = %#v", src)
	}
}

func TestMaxOutputAnthropicDefault(t *testing.T) {
	req := mustParse(t, protoChat, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if got := maxOutputForProtocol(req, protoAnthropic); got != defaultAnthropicMaxTokens {
		t.Fatalf("default max = %d", got)
	}
	if got := maxOutputForProtocol(req, protoChat); got != 0 {
		t.Fatalf("chat max = %d", got)
	}
}

func TestPreviousResponseIDRejected(t *testing.T) {
	_, err := parseClientRequest(protoResponses, []byte(`{"model":"m","input":"x","previous_response_id":"resp_1"}`))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestResponsesDoneOnlyFallback(t *testing.T) {
	out := driveStream(t, protoResponses, protoChat, 0, []string{
		`{"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
		`{"type":"response.output_text.done","item_id":"msg_1","output_index":0,"text":"Hi"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"f","arguments":"{\"a\":1}"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
	})
	for _, want := range []string{`"content":"Hi"`, `"tool_calls"`, `"arguments":"{\"a\":1}"`, `"prompt_tokens":5`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestRelayConvertedBody(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	usage, detail := (&Server{}).relayConvertedBody(rec, req, []byte(`{
	  "id":"chatcmpl-1","model":"m",
	  "choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
	  "usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}
	}`), protoChat, protoAnthropic)
	if detail != "ok" {
		t.Fatalf("detail = %q", detail)
	}
	if usage == nil || usage.PromptTokens != 3 || usage.CompletionTokens != 2 {
		t.Fatalf("usage = %#v", usage)
	}
	body := decodeObject(t, rec.Body.Bytes())
	if body["type"] != "message" || body["stop_reason"] != "end_turn" {
		t.Fatalf("body = %#v", body)
	}
}

func TestRelayConvertedStream(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"c1","choices":[{"delta":{"content":"Hi"}}]}`,
		``,
		`data: {"id":"c1","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	usage, detail := (&Server{}).relayConvertedStream(rec, req, strings.NewReader(upstream), protoChat, protoAnthropic, 4, "m")
	if detail != "ok" {
		t.Fatalf("detail = %q", detail)
	}
	if usage == nil || usage.PromptTokens != 4 || usage.CompletionTokens != 2 {
		t.Fatalf("usage = %#v", usage)
	}
	out := rec.Body.String()
	for _, want := range []string{"message_start", `"text":"Hi"`, `"output_tokens":2`, "message_stop"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}

// driveStream 把上游数据喂给解析器和客户端渲染器，返回渲染后的 SSE。
func driveStream(t *testing.T, upProto, clientProto Protocol, estInput int64, payloads []string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	sw, err := newSSEWriter(rec, clientProto)
	if err != nil {
		t.Fatal(err)
	}
	var renderer streamRenderer
	switch clientProto {
	case protoChat:
		renderer = newChatStreamRenderer(sw, "m", 0)
	case protoResponses:
		renderer = newResponsesStreamRenderer(sw, "m", 0)
	default:
		renderer = newAnthropicStreamRenderer(sw, "m", estInput)
	}
	state := newUpstreamStreamState(upProto)
	for _, p := range payloads {
		for _, ev := range state.parse(sseEvent{Data: p}) {
			switch ev.Kind {
			case "error":
				renderer.fail(ev.Err)
				return rec.Body.String()
			case "finish":
				renderer.finish(ev.Usage, ev.Stop)
				return rec.Body.String()
			default:
				renderer.handle(ev)
			}
		}
	}
	renderer.finish(state.usageIfAny(), state.stopReason())
	return rec.Body.String()
}
