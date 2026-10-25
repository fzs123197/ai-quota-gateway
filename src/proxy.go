package main

// 通用代理链路：/v1/chat/completions、/v1/responses、/v1/messages。
//
// 入口协议与上游协议相同时原样透传（Chat 仍需注入 include_usage）；
// 不同时经 canonRequest / canonResponse / canonEvent 做双向转换。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	s.handleProxy(w, r, protoChat)
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	s.handleProxy(w, r, protoResponses)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.handleProxy(w, r, protoAnthropic)
}

// handleCountTokens 是 Anthropic 的 /v1/messages/count_tokens 端点，
// 用本地估算返回，不调用上游。
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProtocolError(w, protoAnthropic, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeProtocolError(w, protoAnthropic, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	req, err := parseAnthropicRequest(body)
	if err != nil {
		writeProtocolError(w, protoAnthropic, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeProtocolError(w, protoAnthropic, http.StatusBadRequest, "invalid_request_error", "field 'model' is required")
		return
	}
	if _, ok := s.upstreams[req.Model]; !ok {
		writeProtocolError(w, protoAnthropic, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("model %q is not configured on this gateway", req.Model))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"input_tokens": req.estimate().Prompt})
}

func upstreamURL(up *UpstreamConfig, proto Protocol) string {
	base := strings.TrimRight(strings.TrimSpace(up.BaseURL), "/")
	switch proto {
	case protoResponses:
		return base + "/responses"
	case protoAnthropic:
		return base + "/messages"
	default:
		return base + "/chat/completions"
	}
}

func setUpstreamAuth(req *http.Request, up *UpstreamConfig, proto Protocol) {
	switch proto {
	case protoAnthropic:
		req.Header.Set("x-api-key", up.APIKey)
		req.Header.Set("anthropic-version", up.anthropicVersion())
	default:
		req.Header.Set("Authorization", "Bearer "+up.APIKey)
	}
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request, clientProto Protocol) {
	started := time.Now()
	reqID := newRequestID()
	ai := authFrom(r.Context())
	if ai == nil {
		writeProtocolError(w, clientProto, http.StatusUnauthorized, "invalid_request_error", "unauthorized")
		return
	}
	lg := &requestLog{
		ReqID:         reqID,
		KeyID:         ai.Key.ID,
		KeyName:       ai.Key.Name,
		ClientProto:   string(clientProto),
		MonthQuota:    ai.Plan.Month,
		WeekQuota:     ai.Plan.Week,
		FiveHourQuota: ai.Plan.FiveHour,
	}

	if r.Method != http.MethodPost {
		writeProtocolError(w, clientProto, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		lg.Status, lg.Detail = statusBadRequest, "read body: "+err.Error()
		lg.Latency = time.Since(started)
		s.emit(lg)
		writeProtocolError(w, clientProto, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}

	creq, err := parseClientRequest(clientProto, body)
	if err != nil {
		lg.Status, lg.Detail = statusBadRequest, err.Error()
		lg.Latency = time.Since(started)
		s.emit(lg)
		writeProtocolError(w, clientProto, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	lg.Model = creq.Model
	lg.Stream = creq.Stream
	if strings.TrimSpace(creq.Model) == "" {
		lg.Status, lg.Detail = statusBadRequest, "model is empty"
		lg.Latency = time.Since(started)
		s.emit(lg)
		writeProtocolError(w, clientProto, http.StatusBadRequest, "invalid_request_error", "field 'model' is required")
		return
	}

	up, ok := s.upstreams[creq.Model]
	if !ok {
		lg.Status = statusBadRequest
		lg.Detail = "model not configured"
		lg.Latency = time.Since(started)
		s.emit(lg)
		writeProtocolError(w, clientProto, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("model %q is not configured on this gateway (available: %s)", creq.Model, strings.Join(s.models, ", ")))
		return
	}
	upProto := up.protocol()
	lg.Upstream = up.BaseURL
	lg.UpstreamProto = string(upProto)

	if clientProto != upProto && creq.Choices > 1 {
		lg.Status, lg.Detail = statusBadRequest, "n>1 is not supported across protocols"
		lg.Latency = time.Since(started)
		s.emit(lg)
		writeProtocolError(w, clientProto, http.StatusBadRequest, "invalid_request_error",
			"field 'n' greater than 1 is not supported when the request is converted to another protocol")
		return
	}
	if clientProto != upProto {
		creq.MaxOutput = maxOutputForProtocol(creq, upProto)
	}
	creq.Model = up.ModelName

	// ---- 预扣：credits ----
	est := creq.estimate()
	quote, priced := s.prices.Lookup(creq.Model)
	lg.EstTokens = est.Total()
	if !priced {
		lg.Latency = time.Since(started)
		if s.prices.Info().Models == 0 {
			// 价格还没加载出来（常见于启动瞬间）：稍后重试，而不是当成模型配置错误。
			lg.Status, lg.Detail = statusUpstreamError, "pricing not ready"
			s.emit(lg)
			w.Header().Set("Retry-After", "10")
			writeProtocolError(w, clientProto, http.StatusServiceUnavailable, "api_error",
				"pricing is not ready yet; retry shortly")
			return
		}
		lg.Status = statusBadRequest
		lg.Detail = "model has no price"
		s.emit(lg)
		writeProtocolError(w, clientProto, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("model %q has no price; configure upstream.price / price_model / pricing.fallback", creq.Model))
		return
	}
	estCredits := s.prices.EstimateCredits(quote, est)
	res, err := s.qm.Reserve(r.Context(), ai.Key.ID, ai.Plan, reqID, estCredits)
	if err != nil {
		var qe *QuotaExceededError
		if errors.As(err, &qe) {
			lg.Status = statusQuotaExceeded
			lg.Detail = qe.Error()
			s.fillUsage(r.Context(), ai.Key.ID, ai.Plan, lg)
			lg.Latency = time.Since(started)
			s.emit(lg)
			w.Header().Set("Retry-After", "60")
			writeProtocolError(w, clientProto, http.StatusTooManyRequests, "insufficient_quota", qe.Error())
			return
		}
		lg.Status, lg.Detail = statusUpstreamError, "reserve: "+err.Error()
		lg.Latency = time.Since(started)
		s.emit(lg)
		writeProtocolError(w, clientProto, http.StatusServiceUnavailable, "api_error", "quota reservation failed: "+err.Error())
		return
	}
	lg.Reserved = res.credits
	lg.Priced = true

	settled := false
	defer func() {
		if !settled {
			ctx, cancel := accountingCtx()
			defer cancel()
			s.qm.Rollback(ctx, res)
		}
	}()

	// ---- 构造上游请求：同协议透传，跨协议转换 ----
	outBody := body
	if clientProto == upProto {
		if clientProto == protoChat && creq.Stream {
			outBody = ensureIncludeUsage(body)
		}
	} else {
		outBody, err = renderRequest(upProto, creq)
		if err != nil {
			settled = true
			rctx, rcancel := accountingCtx()
			s.qm.Rollback(rctx, res)
			lg.Status, lg.Detail = statusBadRequest, "render request: "+err.Error()
			s.fillUsage(rctx, ai.Key.ID, ai.Plan, lg)
			rcancel()
			lg.Latency = time.Since(started)
			s.emit(lg)
			writeProtocolError(w, clientProto, http.StatusInternalServerError, "api_error", "failed to build upstream request")
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), up.timeout())
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL(up, upProto), bytes.NewReader(outBody))
	if err != nil {
		settled = true
		rctx, rcancel := accountingCtx()
		s.qm.Rollback(rctx, res)
		lg.Status, lg.Detail = statusUpstreamError, "build request: "+err.Error()
		s.fillUsage(rctx, ai.Key.ID, ai.Plan, lg)
		rcancel()
		lg.Latency = time.Since(started)
		s.emit(lg)
		writeProtocolError(w, clientProto, http.StatusInternalServerError, "api_error", "failed to build upstream request")
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setUpstreamAuth(httpReq, up, upProto)
	if upProto == protoAnthropic {
		// 透传 Anthropic 的 beta 开关（如细粒度工具流式）。
		if beta := strings.TrimSpace(r.Header.Get("anthropic-beta")); beta != "" {
			httpReq.Header.Set("anthropic-beta", beta)
		}
	}
	if creq.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	resp, err := s.client.Do(httpReq)
	if err != nil {
		settled = true
		rctx, rcancel := accountingCtx()
		s.qm.Rollback(rctx, res)

		lg.Status = statusUpstreamError
		lg.Detail = err.Error()
		if r.Context().Err() != nil {
			lg.Status = statusClientAbort
		}
		s.fillUsage(rctx, ai.Key.ID, ai.Plan, lg)
		rcancel()
		lg.Latency = time.Since(started)
		s.emit(lg)
		writeProtocolError(w, clientProto, http.StatusBadGateway, "api_error", "upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	lg.UpstreamStatus = resp.StatusCode

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		upBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		settled = true
		rctx, rcancel := accountingCtx()
		s.qm.Rollback(rctx, res)

		lg.Status = statusUpstreamError
		lg.Detail = "upstream returned " + strconv.Itoa(resp.StatusCode)
		s.fillUsage(rctx, ai.Key.ID, ai.Plan, lg)
		rcancel()
		lg.Latency = time.Since(started)
		s.emit(lg)

		if clientProto == upProto {
			if ct := resp.Header.Get("Content-Type"); ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(upBody)
			return
		}
		writeProtocolError(w, clientProto, resp.StatusCode, openAIErrorCode(resp.StatusCode), extractUpstreamError(upBody))
		return
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	isStream := creq.Stream && strings.Contains(contentType, "text/event-stream")

	var usage *Usage
	if clientProto == upProto {
		if isStream {
			usage, lg.Detail = s.relayStreamPassthrough(w, r, resp.Body, upProto)
		} else {
			usage, lg.Detail = s.relayBodyPassthrough(w, r, resp, upProto)
		}
	} else if isStream {
		usage, lg.Detail = s.relayConvertedStream(w, r, resp.Body, upProto, clientProto, est.Prompt, creq.Model)
	} else {
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		if rerr != nil {
			if r.Context().Err() != nil {
				lg.Detail = "client_abort"
			} else {
				lg.Detail = "upstream_error"
				writeProtocolError(w, clientProto, http.StatusBadGateway, "api_error", "read upstream body failed")
			}
			usage = nil
		} else {
			usage, lg.Detail = s.relayConvertedBody(w, r, raw, upProto, clientProto)
		}
	}

	// ---- 记账 ----
	rctx, rcancel := accountingCtx()
	switch {
	case usage != nil && lg.Detail == "ok":
		lg.Usage = usage
		actualCredits := s.prices.Credits(quote, usage)
		actualCost := s.prices.CostNano(quote, usage)
		lg.Delta = s.qm.Settle(rctx, res, actualCredits)
		lg.Credits = actualCredits
		lg.Cost = actualCost
		lg.Status = statusAllowed
		s.store.RecordUsage(rctx, ai.Key.ID, creq.Model, usage.PromptTokens, usage.CompletionTokens, actualCost)
		s.store.TouchKey(rctx, ai.Key)
	case usage != nil && lg.Detail != "ok":
		// 已拿到 usage 但连接中断：按既有策略不计费
		s.qm.Rollback(rctx, res)
		lg.Status = statusClientAbort
		if lg.Detail == "upstream_error" {
			lg.Status = statusUpstreamError
		}
	default:
		s.qm.Rollback(rctx, res)
		lg.Status = statusNoUsage
		if lg.Detail == "client_abort" {
			lg.Status = statusClientAbort
		} else if lg.Detail == "upstream_error" {
			lg.Status = statusUpstreamError
		}
	}
	s.fillUsage(rctx, ai.Key.ID, ai.Plan, lg)
	rcancel()

	settled = true
	lg.Latency = time.Since(started)
	s.emit(lg)
}
