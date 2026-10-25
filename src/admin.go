package main

// 管理台：网关 key、额度方案、用量统计的 REST 接口 + 页面。
// 全部挂在 /admin 下，使用 config 里的 admin_key 鉴权（留空则用 api_key）。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

func (s *Server) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("/admin", s.adminAuth(s.handleAdminPage))
	mux.HandleFunc("/admin/", s.adminAuth(s.handleAdminPage))

	mux.HandleFunc("GET /admin/api/overview", s.adminAuth(s.handleAdminOverview))

	mux.HandleFunc("GET /admin/api/keys", s.adminAuth(s.handleListKeys))
	mux.HandleFunc("POST /admin/api/keys", s.adminAuth(s.handleCreateKey))
	mux.HandleFunc("PATCH /admin/api/keys/{id}", s.adminAuth(s.handleUpdateKey))
	mux.HandleFunc("DELETE /admin/api/keys/{id}", s.adminAuth(s.handleDeleteKey))

	mux.HandleFunc("GET /admin/api/plans", s.adminAuth(s.handleListPlans))
	mux.HandleFunc("POST /admin/api/plans", s.adminAuth(s.handleCreatePlan))
	mux.HandleFunc("PATCH /admin/api/plans/{id}", s.adminAuth(s.handleUpdatePlan))
	mux.HandleFunc("DELETE /admin/api/plans/{id}", s.adminAuth(s.handleDeletePlan))

	mux.HandleFunc("GET /admin/api/usage", s.adminAuth(s.handleUsage))
}

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin" && r.URL.Path != "/admin/" && r.URL.Path != "/admin/index.html" {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(s.adminPath)
	if err != nil {
		http.Error(w, "admin.html not found next to the binary: "+s.adminPath, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// ---------------------------------------------------------------------------
// 响应结构
// ---------------------------------------------------------------------------

// keyOverview 是管理台列表里的一行：key 基本信息 + 当前方案用量。
type keyOverview struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	PlanID    string    `json:"plan_id"`
	PlanName  string    `json:"plan_name"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used,omitempty"`

	MonthUsed  int64 `json:"month_used"`
	MonthQuota int64 `json:"month_quota"`
	WeekUsed   int64 `json:"week_used"`
	WeekQuota  int64 `json:"week_quota"`
	FiveUsed   int64 `json:"five_used"`
	FiveQuota  int64 `json:"five_quota"`
}

type overviewResponse struct {
	Keys    []keyOverview `json:"keys"`
	Plans   []Plan        `json:"plans"`
	Presets []PlanPreset  `json:"presets"`
	Pricing PricingInfo   `json:"pricing"`
	Today   UsagePoint    `json:"today"`
	Counts  struct {
		Keys  int `json:"keys"`
		Plans int `json:"plans"`
	} `json:"counts"`
}

func (s *Server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	keys, err := s.store.ListKeys(ctx)
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	plans, err := s.store.ListPlans(ctx)
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	resp := overviewResponse{
		Plans:   plans,
		Presets: effectivePlanPresets(plans),
		Pricing: s.prices.Info(),
	}
	resp.Keys = s.decorateKeys(ctx, keys, plans)
	resp.Counts.Keys = len(keys)
	resp.Counts.Plans = len(plans)

	now := time.Now()
	if rep, err := s.store.QueryUsage(ctx, now, now, keys, "", ""); err == nil && len(rep.Daily) > 0 {
		resp.Today = rep.Daily[0]
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) decorateKeys(ctx context.Context, keys []APIKey, plans []Plan) []keyOverview {
	planByID := make(map[string]Plan, len(plans))
	for _, p := range plans {
		planByID[p.ID] = p
	}
	out := make([]keyOverview, 0, len(keys))
	for _, k := range keys {
		p := planByID[k.PlanID]
		row := keyOverview{
			ID: k.ID, Name: k.Name, Prefix: k.Prefix, PlanID: k.PlanID, PlanName: p.Name,
			Enabled: k.Enabled, CreatedAt: k.CreatedAt, LastUsed: k.LastUsed,
		}
		if p.Name == "" {
			row.PlanName = "(方案已删除)"
		}
		if snap, err := s.qm.Snapshot(ctx, k.ID, p); err == nil {
			row.MonthUsed, row.MonthQuota = snap.MonthUsed, snap.MonthLimit
			row.WeekUsed, row.WeekQuota = snap.WeekUsed, snap.WeekLimit
			row.FiveUsed, row.FiveQuota = snap.FiveUsed, snap.FiveLimit
		}
		out = append(out, row)
	}
	return out
}

// ---------------------------------------------------------------------------
// key 管理
// ---------------------------------------------------------------------------

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListKeys(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	plans, err := s.store.ListPlans(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": s.decorateKeys(r.Context(), keys, plans)})
}

type createKeyRequest struct {
	Name   string `json:"name"`
	PlanID string `json:"plan_id"`
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req createKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	k, secret, err := s.store.CreateKey(r.Context(), req.Name, req.PlanID)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("创建网关 key", "key", k.Name, "key_id", k.ID, "plan", k.PlanID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":    k,
		"secret": secret, // 只在这里返回一次
	})
}

func (s *Server) handleUpdateKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var patch KeyPatch
	if !decodeJSON(w, r, &patch) {
		return
	}
	k, err := s.store.UpdateKey(r.Context(), id, patch)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrKeyNotFound) {
			status = http.StatusNotFound
		}
		writeAPIError(w, status, err.Error())
		return
	}
	s.log.Info("更新网关 key", "key", k.Name, "key_id", k.ID, "plan", k.PlanID, "enabled", k.Enabled)
	writeJSON(w, http.StatusOK, k)
}

func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteKey(r.Context(), id); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrKeyNotFound) {
			status = http.StatusNotFound
		}
		writeAPIError(w, status, err.Error())
		return
	}
	s.log.Info("删除网关 key", "key_id", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 方案管理
// ---------------------------------------------------------------------------

type planRequest struct {
	Name     string `json:"name"`
	Month    int64  `json:"month"`
	Week     int64  `json:"week"`
	FiveHour int64  `json:"five_hour"`
}

func (p planRequest) toPlan() Plan {
	return Plan{Name: p.Name, Month: p.Month, Week: p.Week, FiveHour: p.FiveHour}
}

func (s *Server) handleListPlans(w http.ResponseWriter, r *http.Request) {
	plans, err := s.store.ListPlans(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": plans})
}

func (s *Server) handleCreatePlan(w http.ResponseWriter, r *http.Request) {
	var req planRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	p, err := s.store.CreatePlan(r.Context(), req.toPlan())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("创建额度方案", "plan", p.Name, "plan_id", p.ID)
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleUpdatePlan(w http.ResponseWriter, r *http.Request) {
	var req planRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	p, err := s.store.UpdatePlan(r.Context(), r.PathValue("id"), req.toPlan())
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrPlanNotFound) {
			status = http.StatusNotFound
		}
		writeAPIError(w, status, err.Error())
		return
	}
	s.log.Info("更新额度方案", "plan", p.Name, "plan_id", p.ID)
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeletePlan(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeletePlan(r.Context(), r.PathValue("id")); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrPlanNotFound) {
			status = http.StatusNotFound
		}
		writeAPIError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 用量统计
// ---------------------------------------------------------------------------

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 30
	}
	if days > 365 {
		days = 365
	}
	keyID := r.URL.Query().Get("key_id")
	model := r.URL.Query().Get("model")

	keys, err := s.store.ListKeys(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	now := time.Now()
	from := now.AddDate(0, 0, -(days - 1))
	report, err := s.store.QueryUsage(r.Context(), from, now, keys, keyID, model)
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	report.From = from.Format("2006-01-02")
	report.To = now.Format("2006-01-02")
	writeJSON(w, http.StatusOK, map[string]any{
		"report":   report,
		"currency": s.cfg.Pricing.Currency,
		"symbol":   s.prices.Symbol(),
	})
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "读取请求体失败")
		return false
	}
	if len(body) == 0 {
		writeAPIError(w, http.StatusBadRequest, "请求体为空")
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeAPIError(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return false
	}
	return true
}
