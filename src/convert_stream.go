package main

// 流式转换：上游 SSE -> canonEvent -> 调用方协议 SSE。
// 同协议时走 relayStreamPassthrough，保持原始字节不变，仅旁路抓 usage。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

var errStreamDone = errors.New("stream done")

// ---------------------------------------------------------------------------
// 上游事件解析
// ---------------------------------------------------------------------------

type upstreamStreamState struct {
	proto    Protocol
	hasUsage bool
	stop     string

	chatUsage *Usage
	respUsage canonUsage
	anthUsage anthropicUsage

	toolSeq        int
	toolByUpstream map[int]int
	blockTool      map[int]int
	textLen        map[string]int
	argsLen        map[int]int
}

func newUpstreamStreamState(proto Protocol) *upstreamStreamState {
	return &upstreamStreamState{
		proto:          proto,
		toolByUpstream: map[int]int{},
		blockTool:      map[int]int{},
		textLen:        map[string]int{},
		argsLen:        map[int]int{},
	}
}

func (st *upstreamStreamState) usageIfAny() *canonUsage {
	if !st.hasUsage {
		return nil
	}
	var u canonUsage
	switch st.proto {
	case protoChat:
		u, _ = usageFromChat(st.chatUsage)
	case protoResponses:
		u = st.respUsage
	case protoAnthropic:
		u, _ = usageFromAnthropic(&st.anthUsage)
	}
	if u.empty() {
		return nil
	}
	return &u
}

func (st *upstreamStreamState) stopReason() string {
	if st.stop == "" {
		return stopEndTurn
	}
	return st.stop
}

func (st *upstreamStreamState) nextTool(upstreamIndex int, id, name string) canonEvent {
	idx := st.toolSeq
	st.toolSeq++
	st.toolByUpstream[upstreamIndex] = idx
	return canonEvent{Kind: "tool_start", ToolIndex: idx, ToolID: id, ToolName: name}
}

// parse 把一条上游 SSE 事件转换成 0..n 个统一事件。
func (st *upstreamStreamState) parse(ev sseEvent) []canonEvent {
	data := strings.TrimSpace(ev.Data)
	if data == "" {
		return nil
	}
	switch st.proto {
	case protoChat:
		return st.parseChat(data)
	case protoResponses:
		return st.parseResponses(ev.Name, data)
	case protoAnthropic:
		return st.parseAnthropic(data)
	}
	return nil
}

func (st *upstreamStreamState) parseChat(data string) []canonEvent {
	if data == "[DONE]" {
		return []canonEvent{{Kind: "finish", Stop: st.stopReason(), Usage: st.usageIfAny()}}
	}
	var chunk struct {
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Content          *string `json:"content"`
				Refusal          *string `json:"refusal"`
				ReasoningContent *string `json:"reasoning_content"`
				Reasoning        *string `json:"reasoning"`
				ToolCalls        []struct {
					Index    *int   `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *Usage `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil
	}
	if chunk.Error != nil && chunk.Error.Message != "" {
		return []canonEvent{{Kind: "error", Err: chunk.Error.Message}}
	}
	if chunk.Usage != nil {
		st.chatUsage = chunk.Usage
		st.hasUsage = true
	}
	var out []canonEvent
	for _, ch := range chunk.Choices {
		if ch.Index != 0 {
			continue // 跨协议只支持单个 choice
		}
		if ch.Delta.Content != nil && *ch.Delta.Content != "" {
			out = append(out, canonEvent{Kind: "text", Text: *ch.Delta.Content})
		}
		if ch.Delta.Refusal != nil && *ch.Delta.Refusal != "" {
			out = append(out, canonEvent{Kind: "text", Text: *ch.Delta.Refusal})
		}
		if ch.Delta.ReasoningContent != nil && *ch.Delta.ReasoningContent != "" {
			out = append(out, canonEvent{Kind: "reasoning", Text: *ch.Delta.ReasoningContent})
		} else if ch.Delta.Reasoning != nil && *ch.Delta.Reasoning != "" {
			out = append(out, canonEvent{Kind: "reasoning", Text: *ch.Delta.Reasoning})
		}
		for _, tc := range ch.Delta.ToolCalls {
			key := 0
			if tc.Index != nil {
				key = *tc.Index
			}
			idx, seen := st.toolByUpstream[key]
			if !seen {
				idx = st.toolSeq
				st.toolSeq++
				st.toolByUpstream[key] = idx
				out = append(out, canonEvent{Kind: "tool_start", ToolIndex: idx, ToolID: tc.ID, ToolName: tc.Function.Name})
			}
			if tc.Function.Arguments != "" {
				out = append(out, canonEvent{Kind: "tool_args", ToolIndex: idx, Text: tc.Function.Arguments})
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			st.stop = mapChatFinish(*ch.FinishReason)
		}
	}
	return out
}

func (st *upstreamStreamState) parseAnthropic(data string) []canonEvent {
	var ev struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
		ContentBlock struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Thinking    string `json:"thinking"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage *anthropicUsage `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return nil
	}
	switch ev.Type {
	case "message_start":
		if ev.Message.Usage != nil {
			st.anthUsage = *ev.Message.Usage
			st.hasUsage = true
		}
	case "content_block_start":
		switch ev.ContentBlock.Type {
		case "text":
			if ev.ContentBlock.Text != "" {
				return []canonEvent{{Kind: "text", Text: ev.ContentBlock.Text}}
			}
		case "thinking":
			if ev.ContentBlock.Thinking != "" {
				return []canonEvent{{Kind: "reasoning", Text: ev.ContentBlock.Thinking}}
			}
		case "tool_use":
			e := st.nextTool(ev.Index, ev.ContentBlock.ID, ev.ContentBlock.Name)
			st.blockTool[ev.Index] = e.ToolIndex
			out := []canonEvent{e}
			initial := strings.TrimSpace(string(ev.ContentBlock.Input))
			if initial != "" && initial != "{}" {
				out = append(out, canonEvent{Kind: "tool_args", ToolIndex: e.ToolIndex, Text: initial})
			}
			return out
		}
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" {
				return []canonEvent{{Kind: "text", Text: ev.Delta.Text}}
			}
		case "thinking_delta":
			if ev.Delta.Thinking != "" {
				return []canonEvent{{Kind: "reasoning", Text: ev.Delta.Thinking}}
			}
		case "input_json_delta":
			idx, ok := st.blockTool[ev.Index]
			if ok && ev.Delta.PartialJSON != "" {
				return []canonEvent{{Kind: "tool_args", ToolIndex: idx, Text: ev.Delta.PartialJSON}}
			}
		}
	case "message_delta":
		if ev.Delta.StopReason != "" {
			st.stop = anthropicStop(ev.Delta.StopReason)
		}
		if ev.Usage != nil {
			if ev.Usage.OutputTokens > 0 {
				st.anthUsage.OutputTokens = ev.Usage.OutputTokens
			}
			if ev.Usage.CacheReadInputTokens > 0 {
				st.anthUsage.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
			}
			st.hasUsage = true
		}
	case "message_stop":
		return []canonEvent{{Kind: "finish", Stop: st.stopReason(), Usage: st.usageIfAny()}}
	case "error":
		msg := "upstream stream error"
		if ev.Error != nil && ev.Error.Message != "" {
			msg = ev.Error.Message
		}
		return []canonEvent{{Kind: "error", Err: msg}}
	}
	return nil
}

func (st *upstreamStreamState) parseResponses(eventName, data string) []canonEvent {
	var ev struct {
		Type        string `json:"type"`
		OutputIndex int    `json:"output_index"`
		ItemID      string `json:"item_id"`
		OutputText  string `json:"text"`
		Arguments   string `json:"arguments"`
		Item        struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"item"`
		Delta    string          `json:"delta"`
		Response json.RawMessage `json:"response"`
		Error    *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return nil
	}
	typ := ev.Type
	if typ == "" {
		typ = eventName
	}
	switch typ {
	case "response.output_item.added":
		if ev.Item.Type == "function_call" {
			e := st.nextTool(ev.OutputIndex, ev.Item.CallID, ev.Item.Name)
			return []canonEvent{e}
		}
	case "response.output_text.delta":
		if ev.Delta != "" {
			st.textLen[ev.ItemID] += len(ev.Delta)
			return []canonEvent{{Kind: "text", Text: ev.Delta}}
		}
	case "response.output_text.done":
		// 某些上游不发 delta，只在 done 里给完整文本。
		if ev.OutputText != "" && st.textLen[ev.ItemID] == 0 {
			return []canonEvent{{Kind: "text", Text: ev.OutputText}}
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if ev.Delta != "" {
			return []canonEvent{{Kind: "reasoning", Text: ev.Delta}}
		}
	case "response.function_call_arguments.delta":
		if idx, ok := st.toolByUpstream[ev.OutputIndex]; ok && ev.Delta != "" {
			st.argsLen[ev.OutputIndex] += len(ev.Delta)
			return []canonEvent{{Kind: "tool_args", ToolIndex: idx, Text: ev.Delta}}
		}
	case "response.function_call_arguments.done":
		if idx, ok := st.toolByUpstream[ev.OutputIndex]; ok && ev.Arguments != "" && st.argsLen[ev.OutputIndex] == 0 {
			st.argsLen[ev.OutputIndex] += len(ev.Arguments)
			return []canonEvent{{Kind: "tool_args", ToolIndex: idx, Text: ev.Arguments}}
		}
	case "response.output_item.done":
		if ev.Item.Type == "function_call" && ev.Item.Arguments != "" && st.argsLen[ev.OutputIndex] == 0 {
			idx, ok := st.toolByUpstream[ev.OutputIndex]
			out := []canonEvent{}
			if !ok {
				e := st.nextTool(ev.OutputIndex, ev.Item.CallID, ev.Item.Name)
				idx = e.ToolIndex
				out = append(out, e)
			}
			st.argsLen[ev.OutputIndex] += len(ev.Item.Arguments)
			out = append(out, canonEvent{Kind: "tool_args", ToolIndex: idx, Text: ev.Item.Arguments})
			return out
		}
	case "response.completed", "response.incomplete", "response.failed":
		resp, err := parseResponsesResponse(ev.Response)
		if err != nil {
			return []canonEvent{{Kind: "error", Err: "invalid response.completed payload"}}
		}
		if resp.Usage != nil {
			st.respUsage = *resp.Usage
			st.hasUsage = true
		}
		if typ == "response.failed" {
			msg := "upstream response failed"
			var failed struct {
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(ev.Response, &failed) == nil && failed.Error != nil && failed.Error.Message != "" {
				msg = failed.Error.Message
			}
			return []canonEvent{{Kind: "error", Err: msg}}
		}
		st.stop = resp.StopReason
		return []canonEvent{{Kind: "finish", Stop: st.stopReason(), Usage: st.usageIfAny()}}
	case "error":
		msg := "upstream stream error"
		if ev.Error != nil && ev.Error.Message != "" {
			msg = ev.Error.Message
		}
		return []canonEvent{{Kind: "error", Err: msg}}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 调用方渲染器
// ---------------------------------------------------------------------------

type streamRenderer interface {
	handle(canonEvent)
	finish(usage *canonUsage, stop string)
	fail(message string)
}

// ---- Chat Completions ----

type chatStreamRenderer struct {
	sw        *sseWriter
	id        string
	model     string
	created   int64
	toolIndex map[int]int
	tools     int
}

func newChatStreamRenderer(sw *sseWriter, model string, _ int64) *chatStreamRenderer {
	r := &chatStreamRenderer{
		sw:        sw,
		id:        newProtocolID("chatcmpl-"),
		model:     model,
		created:   responseNow(),
		toolIndex: map[int]int{},
	}
	r.chunk(map[string]any{"role": "assistant", "content": ""})
	return r
}

func (r *chatStreamRenderer) chunk(delta map[string]any) {
	r.sw.event("", map[string]any{
		"id":      r.id,
		"object":  "chat.completion.chunk",
		"created": r.created,
		"model":   r.model,
		"choices": []any{
			map[string]any{"index": 0, "delta": delta, "finish_reason": nil},
		},
	})
}

func (r *chatStreamRenderer) handle(ev canonEvent) {
	switch ev.Kind {
	case "text":
		r.chunk(map[string]any{"content": ev.Text})
	case "reasoning":
		r.chunk(map[string]any{"reasoning_content": ev.Text})
	case "tool_start":
		idx := r.tools
		r.tools++
		r.toolIndex[ev.ToolIndex] = idx
		id := ev.ToolID
		if id == "" {
			id = newProtocolID("call_")
		}
		r.chunk(map[string]any{
			"tool_calls": []any{
				map[string]any{
					"index": idx,
					"id":    id,
					"type":  "function",
					"function": map[string]any{
						"name":      ev.ToolName,
						"arguments": "",
					},
				},
			},
		})
	case "tool_args":
		idx, ok := r.toolIndex[ev.ToolIndex]
		if !ok {
			return
		}
		r.chunk(map[string]any{
			"tool_calls": []any{
				map[string]any{"index": idx, "function": map[string]any{"arguments": ev.Text}},
			},
		})
	}
}

func (r *chatStreamRenderer) finish(usage *canonUsage, stop string) {
	if r.tools > 0 && (stop == "" || stop == stopEndTurn) {
		stop = stopToolUse
	}
	r.sw.event("", map[string]any{
		"id":      r.id,
		"object":  "chat.completion.chunk",
		"created": r.created,
		"model":   r.model,
		"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": chatFinishFromStop(stop)},
		},
	})
	if usage != nil {
		r.sw.event("", map[string]any{
			"id":      r.id,
			"object":  "chat.completion.chunk",
			"created": r.created,
			"model":   r.model,
			"choices": []any{},
			"usage":   chatUsageJSON(*usage),
		})
	}
	r.sw.raw("data: [DONE]\n\n")
}

func (r *chatStreamRenderer) fail(message string) {
	r.sw.event("", map[string]any{
		"error": map[string]any{"message": message, "type": "api_error", "code": "api_error"},
	})
	r.sw.raw("data: [DONE]\n\n")
}

// ---- Responses ----

type responsesStreamItem struct {
	kind        string // text | reasoning | tool
	outputIndex int
	id          string
	callID      string
	name        string
	text        strings.Builder
	args        strings.Builder
}

type responsesStreamRenderer struct {
	sw         *sseWriter
	id         string
	model      string
	created    int64
	items      []*responsesStreamItem
	textItem   *responsesStreamItem
	reasonItem *responsesStreamItem
	toolItem   map[int]*responsesStreamItem
}

func newResponsesStreamRenderer(sw *sseWriter, model string, _ int64) *responsesStreamRenderer {
	r := &responsesStreamRenderer{
		sw:       sw,
		id:       newProtocolID("resp_"),
		model:    model,
		created:  responseNow(),
		toolItem: map[int]*responsesStreamItem{},
	}
	r.sw.event("response.created", map[string]any{
		"type":     "response.created",
		"response": r.responseObject("in_progress", nil),
	})
	return r
}

func (r *responsesStreamRenderer) responseObject(status string, usage *canonUsage) map[string]any {
	output := make([]any, 0, len(r.items))
	for _, it := range r.items {
		output = append(output, it.json(true))
	}
	obj := map[string]any{
		"id":                 r.id,
		"object":             "response",
		"created_at":         r.created,
		"status":             status,
		"model":              r.model,
		"output":             output,
		"incomplete_details": nil,
	}
	if usage != nil {
		obj["usage"] = responsesUsageJSON(*usage)
	}
	return obj
}

func (it *responsesStreamItem) json(done bool) map[string]any {
	status := "in_progress"
	if done {
		status = "completed"
	}
	switch it.kind {
	case "text":
		return map[string]any{
			"type":   "message",
			"id":     it.id,
			"status": status,
			"role":   "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": it.text.String(), "annotations": []any{}},
			},
		}
	case "reasoning":
		return map[string]any{
			"type":   "reasoning",
			"id":     it.id,
			"status": status,
			"summary": []any{
				map[string]any{"type": "summary_text", "text": it.text.String()},
			},
		}
	default:
		return map[string]any{
			"type":      "function_call",
			"id":        it.id,
			"call_id":   it.callID,
			"name":      it.name,
			"arguments": it.args.String(),
			"status":    status,
		}
	}
}

func (r *responsesStreamRenderer) addItem(kind string) *responsesStreamItem {
	it := &responsesStreamItem{kind: kind, outputIndex: len(r.items)}
	switch kind {
	case "text":
		it.id = newProtocolID("msg_")
	case "reasoning":
		it.id = newProtocolID("rs_")
	default:
		it.id = newProtocolID("fc_")
	}
	r.items = append(r.items, it)
	return it
}

func (r *responsesStreamRenderer) handle(ev canonEvent) {
	switch ev.Kind {
	case "text":
		if r.textItem == nil {
			it := r.addItem("text")
			r.textItem = it
			r.sw.event("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": it.outputIndex, "item": it.json(false),
			})
			r.sw.event("response.content_part.added", map[string]any{
				"type": "response.content_part.added", "item_id": it.id, "output_index": it.outputIndex,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			})
		}
		it := r.textItem
		it.text.WriteString(ev.Text)
		r.sw.event("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": it.id, "output_index": it.outputIndex,
			"content_index": 0, "delta": ev.Text, "logprobs": []any{},
		})
	case "reasoning":
		if r.reasonItem == nil {
			it := r.addItem("reasoning")
			r.reasonItem = it
			r.sw.event("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": it.outputIndex, "item": it.json(false),
			})
			r.sw.event("response.reasoning_summary_part.added", map[string]any{
				"type": "response.reasoning_summary_part.added", "item_id": it.id, "output_index": it.outputIndex,
				"summary_index": 0,
				"part":          map[string]any{"type": "summary_text", "text": ""},
			})
		}
		it := r.reasonItem
		it.text.WriteString(ev.Text)
		r.sw.event("response.reasoning_summary_text.delta", map[string]any{
			"type": "response.reasoning_summary_text.delta", "item_id": it.id, "output_index": it.outputIndex,
			"summary_index": 0, "delta": ev.Text,
		})
	case "tool_start":
		it := r.addItem("tool")
		it.callID = ev.ToolID
		if it.callID == "" {
			it.callID = newProtocolID("call_")
		}
		it.name = ev.ToolName
		r.toolItem[ev.ToolIndex] = it
		r.sw.event("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": it.outputIndex, "item": it.json(false),
		})
	case "tool_args":
		it, ok := r.toolItem[ev.ToolIndex]
		if !ok {
			return
		}
		it.args.WriteString(ev.Text)
		r.sw.event("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": it.id,
			"output_index": it.outputIndex, "delta": ev.Text,
		})
	}
}

func (r *responsesStreamRenderer) finish(usage *canonUsage, stop string) {
	for _, it := range r.items {
		switch it.kind {
		case "text":
			r.sw.event("response.output_text.done", map[string]any{
				"type": "response.output_text.done", "item_id": it.id, "output_index": it.outputIndex,
				"content_index": 0, "text": it.text.String(), "logprobs": []any{},
			})
			r.sw.event("response.content_part.done", map[string]any{
				"type": "response.content_part.done", "item_id": it.id, "output_index": it.outputIndex,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": it.text.String(), "annotations": []any{}},
			})
		case "reasoning":
			r.sw.event("response.reasoning_summary_text.done", map[string]any{
				"type": "response.reasoning_summary_text.done", "item_id": it.id, "output_index": it.outputIndex,
				"summary_index": 0, "text": it.text.String(),
			})
			r.sw.event("response.reasoning_summary_part.done", map[string]any{
				"type": "response.reasoning_summary_part.done", "item_id": it.id, "output_index": it.outputIndex,
				"summary_index": 0,
				"part":          map[string]any{"type": "summary_text", "text": it.text.String()},
			})
		case "tool":
			r.sw.event("response.function_call_arguments.done", map[string]any{
				"type": "response.function_call_arguments.done", "item_id": it.id,
				"output_index": it.outputIndex, "arguments": it.args.String(),
			})
		}
		r.sw.event("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": it.outputIndex, "item": it.json(true),
		})
	}
	status := "completed"
	name := "response.completed"
	if stop == stopMaxTokens || stop == stopRefusal {
		status = "incomplete"
		name = "response.incomplete"
	}
	obj := r.responseObject(status, usage)
	if status == "incomplete" {
		reason := "max_output_tokens"
		if stop == stopRefusal {
			reason = "content_filter"
		}
		obj["incomplete_details"] = map[string]any{"reason": reason}
	}
	r.sw.event(name, map[string]any{
		"type":     name,
		"response": obj,
	})
}

func (r *responsesStreamRenderer) fail(message string) {
	r.sw.event("error", map[string]any{
		"type":    "error",
		"code":    "api_error",
		"message": message,
		"param":   nil,
	})
}

// ---- Anthropic Messages ----

type anthropicStreamRenderer struct {
	sw           *sseWriter
	id           string
	model        string
	estInput     int64
	blocks       int
	current      string
	currentIndex int
	toolBlocks   map[int]int
}

func newAnthropicStreamRenderer(sw *sseWriter, model string, estInput int64) *anthropicStreamRenderer {
	r := &anthropicStreamRenderer{
		sw:         sw,
		id:         newProtocolID("msg_"),
		model:      model,
		estInput:   estInput,
		toolBlocks: map[int]int{},
	}
	r.sw.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            r.id,
			"type":          "message",
			"role":          "assistant",
			"model":         r.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": r.estInput, "output_tokens": 0},
		},
	})
	return r
}

func (r *anthropicStreamRenderer) closeBlock() {
	if r.current == "" {
		return
	}
	r.sw.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": r.currentIndex})
	r.current = ""
}

func (r *anthropicStreamRenderer) openText() {
	if r.current == "text" {
		return
	}
	r.closeBlock()
	idx := r.blocks
	r.blocks++
	r.sw.event("content_block_start", map[string]any{
		"type": "content_block_start", "index": idx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	r.current, r.currentIndex = "text", idx
}

func (r *anthropicStreamRenderer) handle(ev canonEvent) {
	switch ev.Kind {
	case "text":
		r.openText()
		r.sw.event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": r.currentIndex,
			"delta": map[string]any{"type": "text_delta", "text": ev.Text},
		})
	case "tool_start":
		r.closeBlock()
		idx := r.blocks
		r.blocks++
		id := ev.ToolID
		if id == "" {
			id = newProtocolID("toolu_")
		}
		r.toolBlocks[ev.ToolIndex] = idx
		r.sw.event("content_block_start", map[string]any{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]any{
				"type": "tool_use", "id": id, "name": ev.ToolName, "input": map[string]any{},
			},
		})
		r.current, r.currentIndex = "tool", idx
	case "tool_args":
		idx, ok := r.toolBlocks[ev.ToolIndex]
		if !ok {
			return
		}
		r.sw.event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.Text},
		})
	}
}

func (r *anthropicStreamRenderer) finish(usage *canonUsage, stop string) {
	if r.blocks == 0 {
		// Anthropic 客户端普遍不接受空 content，补一个空文本块。
		r.openText()
	}
	r.closeBlock()
	stop = anthropicStop(stop)
	if len(r.toolBlocks) > 0 && stop == stopEndTurn {
		stop = stopToolUse
	}
	usageJSON := map[string]any{"output_tokens": int64(0)}
	if usage != nil {
		usageJSON["output_tokens"] = usage.Output
		if usage.Cached > 0 {
			usageJSON["cache_read_input_tokens"] = usage.Cached
		}
	}
	r.sw.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": usageJSON,
	})
	r.sw.event("message_stop", map[string]any{"type": "message_stop"})
}

func (r *anthropicStreamRenderer) fail(message string) {
	r.sw.event("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": message},
	})
}

// ---------------------------------------------------------------------------
// 转换主循环
// ---------------------------------------------------------------------------

func (s *Server) relayConvertedStream(w http.ResponseWriter, r *http.Request, body io.Reader,
	upProto, clientProto Protocol, estInput int64, model string) (*Usage, string) {

	sw, err := newSSEWriter(w, clientProto)
	if err != nil {
		writeProtocolError(w, clientProto, http.StatusInternalServerError, "api_error", "streaming unsupported by server")
		return nil, "upstream_error"
	}
	var renderer streamRenderer
	switch clientProto {
	case protoChat:
		renderer = newChatStreamRenderer(sw, model, 0)
	case protoResponses:
		renderer = newResponsesStreamRenderer(sw, model, 0)
	default:
		renderer = newAnthropicStreamRenderer(sw, model, estInput)
	}

	state := newUpstreamStreamState(upProto)
	detail := "ok"
	sawFinish := false

	err = readSSE(body, func(ev sseEvent) error {
		if r.Context().Err() != nil {
			return r.Context().Err()
		}
		for _, ce := range state.parse(ev) {
			switch ce.Kind {
			case "error":
				renderer.fail(ce.Err)
				detail = "upstream_error"
				return errStreamDone
			case "finish":
				renderer.finish(ce.Usage, ce.Stop)
				sawFinish = true
				return errStreamDone
			default:
				renderer.handle(ce)
			}
			if sw.err != nil {
				detail = "client_abort"
				return errStreamDone
			}
		}
		if sw.err != nil {
			detail = "client_abort"
			return errStreamDone
		}
		return nil
	})

	if err != nil && !errors.Is(err, errStreamDone) {
		if r.Context().Err() != nil {
			detail = "client_abort"
		} else if detail == "ok" {
			detail = "upstream_error"
			renderer.fail("upstream stream error: " + err.Error())
		}
	}
	if !sawFinish && detail == "ok" {
		renderer.finish(state.usageIfAny(), state.stopReason())
	}
	if sw.err != nil {
		detail = "client_abort"
	}
	return canonUsageToUsage(state.usageIfAny()), detail
}

// ---------------------------------------------------------------------------
// 同协议透传：保持上游字节原样，仅旁路抓 usage
// ---------------------------------------------------------------------------

type passthroughUsageCapture struct {
	proto Protocol
	chat  *Usage
	resp  *responsesUsage
	anth  anthropicUsage
	has   bool
	err   bool
}

func newPassthroughUsageCapture(proto Protocol) *passthroughUsageCapture {
	return &passthroughUsageCapture{proto: proto}
}

func (c *passthroughUsageCapture) feed(line []byte) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		if bytes.HasPrefix(s, []byte("event: error")) {
			c.err = true
		}
		return
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	switch c.proto {
	case protoChat:
		var chunk struct {
			Usage *Usage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(payload, &chunk) == nil {
			if chunk.Usage != nil {
				c.chat = chunk.Usage
				c.has = true
			}
			if chunk.Error != nil && chunk.Error.Message != "" {
				c.err = true
			}
		}
	case protoResponses:
		var ev struct {
			Type     string               `json:"type"`
			Response *responsesResponseIn `json:"response"`
		}
		if json.Unmarshal(payload, &ev) == nil {
			if ev.Response != nil && ev.Response.Usage != nil &&
				(ev.Type == "response.completed" || ev.Type == "response.incomplete" || ev.Type == "response.failed") {
				c.resp = ev.Response.Usage
				c.has = true
			}
			if ev.Type == "error" || ev.Type == "response.failed" {
				c.err = true
			}
		}
	case protoAnthropic:
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Usage *anthropicUsage `json:"usage"`
			} `json:"message"`
			Usage *anthropicUsage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(payload, &ev) == nil {
			switch ev.Type {
			case "message_start":
				if ev.Message.Usage != nil {
					c.anth = *ev.Message.Usage
					c.has = true
				}
			case "message_delta":
				if ev.Usage != nil {
					if ev.Usage.OutputTokens > 0 {
						c.anth.OutputTokens = ev.Usage.OutputTokens
					}
					if ev.Usage.CacheReadInputTokens > 0 {
						c.anth.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
					}
					c.has = true
				}
			case "error":
				c.err = true
			}
		}
	}
}

func (c *passthroughUsageCapture) result() *Usage {
	if !c.has {
		return nil
	}
	switch c.proto {
	case protoChat:
		return c.chat
	case protoResponses:
		u, ok := usageFromResponses(c.resp)
		if !ok {
			return nil
		}
		return u.toUsage()
	case protoAnthropic:
		u, ok := usageFromAnthropic(&c.anth)
		if !ok {
			return nil
		}
		return u.toUsage()
	}
	return nil
}

func (s *Server) relayStreamPassthrough(w http.ResponseWriter, r *http.Request, body io.Reader, proto Protocol) (*Usage, string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProtocolError(w, proto, http.StatusInternalServerError, "api_error", "streaming unsupported by server")
		return nil, "upstream_error"
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	capture := newPassthroughUsageCapture(proto)
	br := bufio.NewReaderSize(body, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			capture.feed(line)
			if _, werr := w.Write(line); werr != nil {
				return capture.result(), "client_abort"
			}
			flusher.Flush()
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if capture.err {
					return capture.result(), "upstream_error"
				}
				return capture.result(), "ok"
			}
			if r.Context().Err() != nil {
				return capture.result(), "client_abort"
			}
			return capture.result(), "upstream_error"
		}
		if r.Context().Err() != nil {
			return capture.result(), "client_abort"
		}
	}
}

func (s *Server) relayBodyPassthrough(w http.ResponseWriter, r *http.Request, resp *http.Response, proto Protocol) (*Usage, string) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		if r.Context().Err() != nil {
			return nil, "client_abort"
		}
		writeProtocolError(w, proto, http.StatusBadGateway, "api_error", "read upstream body failed")
		return nil, "upstream_error"
	}
	usage := extractUsageFromBody(proto, raw)
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	if _, werr := w.Write(raw); werr != nil {
		return canonUsageToUsage(usage), "client_abort"
	}
	return canonUsageToUsage(usage), "ok"
}

func (s *Server) relayConvertedBody(w http.ResponseWriter, r *http.Request, raw []byte, upProto, clientProto Protocol) (*Usage, string) {
	resp, err := parseCanonicalResponse(upProto, raw)
	if err != nil {
		writeProtocolError(w, clientProto, http.StatusBadGateway, "api_error",
			"failed to parse upstream response: "+err.Error())
		return nil, "upstream_error"
	}
	out, err := renderCanonicalResponse(clientProto, resp)
	if err != nil {
		writeProtocolError(w, clientProto, http.StatusInternalServerError, "api_error",
			"failed to encode response: "+err.Error())
		return nil, "upstream_error"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, werr := w.Write(out); werr != nil {
		return canonUsageToUsage(resp.Usage), "client_abort"
	}
	return canonUsageToUsage(resp.Usage), "ok"
}
