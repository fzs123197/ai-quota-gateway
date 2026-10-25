package main

// 计价与汇率
//
// 价格来自多个公开目录（models5 / models.dev / openrouter），按 config 中 sources 的顺序
// 逐个尝试，第一个命中的源胜出。所有价格在内部统一折算成「美元 / 百万 token」。
//
// 汇率不写死在代码或配置里：启动时实时拉取，落盘缓存，定时刷新；拉取失败时用上次缓存，
// 再不行才用配置里的 fallback_rate 兜底。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 1 credit = 0.000001 USD。模型倍率（credits/token）在数值上等于
// 该模型的美元单价 / 1M tokens，因此倍率完全由价格自动得出。
const creditUnitUSD = 0.000001

// ---------------------------------------------------------------------------
// 基础类型
// ---------------------------------------------------------------------------

// Price 是「美元 / 百万 token」单价。Tiers 按 Size 升序，输入超过某档则用该档价格。
type Price struct {
	Input     float64     `json:"input"`
	Output    float64     `json:"output"`
	CacheRead float64     `json:"cache_read"`
	Tiers     []PriceTier `json:"tiers,omitempty"`
}

type PriceTier struct {
	Size      int64   `json:"size"`
	Input     float64 `json:"input"`
	Output    float64 `json:"output"`
	CacheRead float64 `json:"cache_read"`
}

// forPrompt 按输入 token 数选出适用档位。
func (p Price) forPrompt(n int64) Price {
	out := Price{Input: p.Input, Output: p.Output, CacheRead: p.CacheRead}
	for _, t := range p.Tiers {
		if n > t.Size {
			out.Input, out.Output, out.CacheRead = t.Input, t.Output, t.CacheRead
		}
	}
	return out
}

// usd 返回本次请求的美元成本。命中缓存的部分按 cache_read 单价计。
func (p Price) usd(u *Usage) float64 {
	if u == nil {
		return 0
	}
	pp := p.forPrompt(u.PromptTokens)
	var total float64
	if hit := u.cacheHitTokens(); hit > 0 && pp.CacheRead > 0 {
		miss := u.PromptTokens - hit
		if miss < 0 {
			miss = 0
		}
		total += float64(hit) / 1e6 * pp.CacheRead
		total += float64(miss) / 1e6 * pp.Input
	} else {
		total += float64(u.PromptTokens) / 1e6 * pp.Input
	}
	total += float64(u.CompletionTokens) / 1e6 * pp.Output
	return total
}

// Quote 是某个网关模型的定价结果。Price 内部统一为美元。
type Quote struct {
	Price    Price
	Provider string // 价格源里的厂商 id，或 manual / fallback
	Source   string // 命中的价格源名称
}

// ---------------------------------------------------------------------------
// 各价格源的 JSON 结构
// ---------------------------------------------------------------------------

// catalog（models5 / models.dev）：
//
//	providers.<providerID>.models.<modelID>.cost = {input, output, cache_read, tiers[]}
type catalogDoc struct {
	Providers map[string]catalogProvider `json:"providers"`
}

type catalogProvider struct {
	ID     string                  `json:"id"`
	Name   string                  `json:"name"`
	API    string                  `json:"api"`
	Models map[string]catalogModel `json:"models"`
}

type catalogModel struct {
	ID   string       `json:"id"`
	Name string       `json:"name"`
	Cost *catalogCost `json:"cost"`
}

type catalogCost struct {
	Input     float64       `json:"input"`
	Output    float64       `json:"output"`
	CacheRead float64       `json:"cache_read"`
	Tiers     []catalogTier `json:"tiers"`
}

type catalogTier struct {
	Input     float64 `json:"input"`
	Output    float64 `json:"output"`
	CacheRead float64 `json:"cache_read"`
	Tier      struct {
		Type string `json:"type"`
		Size int64  `json:"size"`
	} `json:"tier"`
}

// openrouter：data[].pricing = {prompt, completion, input_cache_read}，单位是「美元 / token」
type openRouterDoc struct {
	Data []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Pricing struct {
			Prompt          string `json:"prompt"`
			Completion      string `json:"completion"`
			InputCacheRead  string `json:"input_cache_read"`
			InternalReasoni string `json:"internal_reasoning"`
		} `json:"pricing"`
	} `json:"data"`
}

// sourceIndex 是某个价格源解析后的索引。
type sourceIndex struct {
	name      string
	providers map[string]map[string]Price // providerID -> modelID -> price
	flat      map[string]Price            // modelID（含 "provider/model" 的后半段）
}

func (si *sourceIndex) empty() bool { return len(si.providers) == 0 && len(si.flat) == 0 }

func (si *sourceIndex) lookup(provider, model string) (Price, string, bool) {
	if provider != "" {
		if models, ok := si.providers[provider]; ok {
			if p, ok := models[model]; ok {
				return p, provider, true
			}
		}
	}
	if p, ok := si.flat[model]; ok {
		return p, "", true
	}
	return Price{}, "", false
}

// ---------------------------------------------------------------------------
// 汇率（实时获取）
// ---------------------------------------------------------------------------

type FXConfig struct {
	URL          string  `json:"url"`
	Format       string  `json:"format"` // rates（er-api / frankfurter）| fawazahmed
	RefreshHours int     `json:"refresh_hours"`
	FallbackRate float64 `json:"fallback_rate"` // 仅在实时汇率长期不可用时兜底，0 表示不兜底
}

// FXRates 维护 USD -> CNY 的实时汇率。
type FXRates struct {
	cfg    FXConfig
	client *http.Client
	log    *slog.Logger

	mu      sync.RWMutex
	rate    float64
	updated time.Time
	source  string
	lastErr error
}

type FXInfo struct {
	Rate      float64 `json:"rate"`
	UpdatedAt string  `json:"updated_at,omitempty"`
	Source    string  `json:"source,omitempty"`
	LastError string  `json:"last_error,omitempty"`
}

func NewFXRates(cfg FXConfig, logger *slog.Logger) *FXRates {
	return &FXRates{
		cfg: cfg,
		log: logger,
		client: &http.Client{Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConns:        4,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		}},
	}
}

// Rate 返回 USD -> CNY 汇率；0 表示当前不可用。
func (f *FXRates) Rate() float64 {
	if f == nil {
		return 0
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.rate
}

func (f *FXRates) Info() FXInfo {
	if f == nil {
		return FXInfo{}
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	info := FXInfo{Rate: round6(f.rate), Source: f.source}
	if !f.updated.IsZero() {
		info.UpdatedAt = f.updated.Format(time.RFC3339)
	}
	if f.lastErr != nil {
		info.LastError = f.lastErr.Error()
	}
	return info
}

func (f *FXRates) cachePath() string {
	return filepath.Join(filepath.Dir(configPath()), fxCacheFile)
}

// Load 拉取实时汇率；失败时退回磁盘缓存，再退回 fallback_rate。
func (f *FXRates) Load(ctx context.Context) error {
	if f == nil {
		return nil
	}
	rate, err := f.fetch(ctx)
	if err != nil {
		// 1) 磁盘缓存
		if cached, cerr := readFXCcache(f.cachePath()); cerr == nil && cached > 0 {
			f.set(cached, "cache", err)
			f.log.Warn("获取实时汇率失败，使用上次缓存", "rate", cached, "err", err)
			return err
		}
		// 2) 配置兜底
		if f.cfg.FallbackRate > 0 {
			f.set(f.cfg.FallbackRate, "fallback", err)
			f.log.Warn("获取实时汇率失败，使用 fallback_rate", "rate", f.cfg.FallbackRate, "err", err)
			return err
		}
		f.mu.Lock()
		f.lastErr = err
		f.mu.Unlock()
		f.log.Error("获取实时汇率失败且无可用兜底，CNY 计费将暂停", "err", err)
		return err
	}
	f.set(rate, "live", nil)
	_ = writeFXCcache(f.cachePath(), rate)
	f.log.Info("实时汇率已更新", "usd_cny", rate, "source", f.cfg.URL)
	return nil
}

func (f *FXRates) set(rate float64, source string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rate = rate
	f.source = source
	f.updated = time.Now()
	f.lastErr = err
}

func (f *FXRates) fetch(ctx context.Context) (float64, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, f.cfg.URL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("汇率接口返回 HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	return parseFXRates(raw, f.cfg.Format)
}

func parseFXRates(raw []byte, format string) (float64, error) {
	switch strings.ToLower(format) {
	case "fawazahmed":
		var doc struct {
			USD map[string]float64 `json:"usd"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return 0, err
		}
		if v := doc.USD["cny"]; v > 0 {
			return v, nil
		}
		return 0, errors.New("汇率响应中没有 usd.cny")
	default: // rates：open.er-api.com / frankfurter 等
		var doc struct {
			Result string             `json:"result"`
			Rates  map[string]float64 `json:"rates"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return 0, err
		}
		if doc.Result != "" && !strings.EqualFold(doc.Result, "success") {
			return 0, fmt.Errorf("汇率接口返回 %q", doc.Result)
		}
		if v := doc.Rates["CNY"]; v > 0 {
			return v, nil
		}
		return 0, errors.New("汇率响应中没有 CNY")
	}
}

// StartAutoRefresh 定时刷新汇率（默认 6 小时）。
func (f *FXRates) StartAutoRefresh(ctx context.Context) {
	if f == nil || f.cfg.URL == "" {
		return
	}
	interval := time.Duration(f.cfg.RefreshHours) * time.Hour
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = f.Load(ctx)
			}
		}
	}()
}

func readFXCcache(path string) (float64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var doc struct {
		Rate float64 `json:"usd_cny"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, err
	}
	return doc.Rate, nil
}

func writeFXCcache(path string, rate float64) error {
	body, _ := json.Marshal(map[string]any{
		"usd_cny":    rate,
		"updated_at": time.Now().Format(time.RFC3339),
	})
	return os.WriteFile(path, body, 0o644)
}

// ---------------------------------------------------------------------------
// 价格簿
// ---------------------------------------------------------------------------

type PriceSource struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Format  string `json:"format"` // catalog | openrouter
	Enabled *bool  `json:"enabled,omitempty"`
}

func (s PriceSource) enabled() bool { return s.Enabled == nil || *s.Enabled }

// PriceBook 持有全部已配置模型的单价，支持多源合并 + 定期刷新。
type PriceBook struct {
	cfg    PricingConfig
	client *http.Client
	log    *slog.Logger
	fx     *FXRates

	mu      sync.RWMutex
	quotes  map[string]Quote
	missing []string
	sources []string // 实际生效的源名称
	updated time.Time
	lastErr error
}

func NewPriceBook(cfg PricingConfig, logger *slog.Logger) *PriceBook {
	pb := &PriceBook{
		cfg: cfg,
		log: logger,
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 90 * time.Second,
		}},
		quotes: make(map[string]Quote),
	}
	if cfg.Enabled {
		pb.fx = NewFXRates(cfg.FX, logger)
	}
	return pb
}

func (pb *PriceBook) Enabled() bool { return pb.cfg.Enabled }

// Symbol 返回展示货币符号。
func (pb *PriceBook) Symbol() string {
	if pb.cfg.Currency == "CNY" {
		return "¥"
	}
	return "$"
}

// toDisplay 把美元换算成配置的展示货币。
func (pb *PriceBook) toDisplay(usd float64) float64 {
	if pb.cfg.Currency != "CNY" {
		return usd
	}
	rate := pb.fx.Rate()
	if rate <= 0 {
		return 0
	}
	return usd * rate
}

// fromDisplay 把手动配置的（展示货币）价格换算回美元。
func (pb *PriceBook) fromDisplay(v float64) float64 {
	if pb.cfg.Currency != "CNY" {
		return v
	}
	rate := pb.fx.Rate()
	if rate <= 0 {
		return 0
	}
	return v / rate
}

func (pb *PriceBook) Lookup(model string) (Quote, bool) {
	pb.mu.RLock()
	defer pb.mu.RUnlock()
	q, ok := pb.quotes[model]
	return q, ok
}

// CostNano 把一次真实 usage 换算成金额（纳单位，展示货币）。
func (pb *PriceBook) CostNano(q Quote, u *Usage) int64 {
	if !pb.cfg.Enabled || u == nil {
		return 0
	}
	return moneyToNano(pb.toDisplay(q.Price.usd(u)))
}

func usdToCredits(usd float64) int64 {
	if usd <= 0 {
		return 0
	}
	c := int64(math.Round(usd / creditUnitUSD))
	if c < 1 {
		c = 1
	}
	return c
}

// Credits 把一次真实 usage 换算成 credits。
func (pb *PriceBook) Credits(q Quote, u *Usage) int64 {
	if !pb.cfg.Enabled || u == nil {
		return 0
	}
	return usdToCredits(q.Price.usd(u))
}

// EstimateCredits 用预估 token 计算预扣 credits。
func (pb *PriceBook) EstimateCredits(q Quote, est tokenEstimate) int64 {
	if !pb.cfg.Enabled {
		return 0
	}
	return usdToCredits(q.Price.usd(&Usage{
		PromptTokens:     est.Prompt,
		CompletionTokens: est.Completion,
	}))
}

// CreditUnit 返回 1 credit 折合多少展示货币。
func (pb *PriceBook) CreditUnit() float64 { return pb.toDisplay(creditUnitUSD) }

func (pb *PriceBook) CreditUnitUSD() float64 { return creditUnitUSD }

func (pb *PriceBook) cachePath() string {
	return filepath.Join(filepath.Dir(configPath()), pricingCacheFile)
}

// LoadCache 用本地缓存快速恢复价格（启动秒开）。
func (pb *PriceBook) LoadCache(ups []UpstreamConfig) bool {
	raw, err := os.ReadFile(pb.cachePath())
	if err != nil || len(raw) == 0 {
		return false
	}
	if err := pb.apply(raw, ups); err != nil {
		pb.log.Warn("本地价格缓存不可用，已忽略", "path", pb.cachePath(), "err", err)
		return false
	}
	return true
}

// Load 拉取全部价格源并重建单价表。
func (pb *PriceBook) Load(ctx context.Context, ups []UpstreamConfig) error {
	err := pb.fetchAll(ctx, ups)
	pb.mu.Lock()
	pb.lastErr = err
	pb.mu.Unlock()
	return err
}

func (pb *PriceBook) fetchAll(ctx context.Context, ups []UpstreamConfig) error {
	sources := pb.cfg.effectiveSources()
	if len(sources) == 0 {
		return errors.New("未配置价格源（pricing.sources）")
	}

	type result struct {
		idx *sourceIndex
		err error
	}
	results := make([]result, len(sources))
	var wg sync.WaitGroup
	for i, src := range sources {
		wg.Add(1)
		go func(i int, src PriceSource) {
			defer wg.Done()
			idx, err := pb.fetchSource(ctx, src)
			results[i] = result{idx: idx, err: err}
		}(i, src)
	}
	wg.Wait()

	var indexes []*sourceIndex
	var firstErr error
	for i, r := range results {
		if r.err != nil {
			pb.log.Warn("价格源拉取失败", "source", sources[i].Name, "url", sources[i].URL, "err", r.err)
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		indexes = append(indexes, r.idx)
	}
	if len(indexes) == 0 {
		// 全部失败 → 退回磁盘缓存
		if raw, err := os.ReadFile(pb.cachePath()); err == nil && len(raw) > 0 {
			if aerr := pb.apply(raw, ups); aerr == nil {
				pb.log.Warn("全部价格源不可用，已改用本地缓存", "cache", pb.cachePath())
				return nil
			}
		}
		if firstErr == nil {
			firstErr = errors.New("没有可用的价格源")
		}
		return firstErr
	}

	// 缓存只存第一优先源的原始响应，用于下次启动秒开
	if raw, err := pb.download(ctx, sources[0]); err == nil && len(raw) > 0 {
		if werr := os.WriteFile(pb.cachePath(), raw, 0o644); werr != nil {
			pb.log.Warn("写入价格缓存失败", "err", werr)
		}
	}

	return pb.applyIndexes(indexes, ups)
}

func (pb *PriceBook) fetchSource(ctx context.Context, src PriceSource) (*sourceIndex, error) {
	raw, err := pb.download(ctx, src)
	if err != nil {
		return nil, err
	}
	return parseSource(raw, src)
}

func (pb *PriceBook) download(ctx context.Context, src PriceSource) ([]byte, error) {
	timeout := time.Duration(pb.cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, src.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := pb.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, catalogMaxBytes))
}

// parseSource 按格式解析单个价格源。
func parseSource(raw []byte, src PriceSource) (*sourceIndex, error) {
	switch strings.ToLower(src.Format) {
	case "openrouter":
		return parseOpenRouter(raw, src.Name)
	case "modelsdev":
		// models.dev/api.json：根对象本身就是 provider 映射
		var providers map[string]catalogProvider
		if err := json.Unmarshal(raw, &providers); err != nil {
			return nil, fmt.Errorf("解析 models.dev 失败: %w", err)
		}
		return buildIndex(providers, src.Name)
	case "", "catalog":
		return parseCatalog(raw, src.Name)
	default:
		return nil, fmt.Errorf("未知的价格源格式 %q", src.Format)
	}
}

func parseCatalog(raw []byte, name string) (*sourceIndex, error) {
	var doc catalogDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("解析 catalog 失败: %w", err)
	}
	return buildIndex(doc.Providers, name)
}

func buildIndex(providers map[string]catalogProvider, name string) (*sourceIndex, error) {
	if len(providers) == 0 {
		return nil, errors.New("价格源中没有 providers")
	}
	idx := &sourceIndex{name: name, providers: make(map[string]map[string]Price), flat: map[string]Price{}}
	for pid, p := range providers {
		models := make(map[string]Price, len(p.Models))
		for mid, m := range p.Models {
			if m.Cost == nil {
				continue
			}
			models[mid] = priceFromCatalogCost(m.Cost)
		}
		if len(models) > 0 {
			idx.providers[pid] = models
		}
	}
	if len(idx.providers) == 0 {
		return nil, errors.New("价格源中没有带 cost 的模型")
	}
	return idx, nil
}

func parseOpenRouter(raw []byte, name string) (*sourceIndex, error) {
	var doc openRouterDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("解析 openrouter 失败: %w", err)
	}
	if len(doc.Data) == 0 {
		return nil, errors.New("openrouter 返回空列表")
	}
	idx := &sourceIndex{name: name, providers: map[string]map[string]Price{}, flat: map[string]Price{}}
	for _, m := range doc.Data {
		in, _ := strconv.ParseFloat(m.Pricing.Prompt, 64)
		out, _ := strconv.ParseFloat(m.Pricing.Completion, 64)
		cr, _ := strconv.ParseFloat(m.Pricing.InputCacheRead, 64)
		if in <= 0 && out <= 0 {
			continue
		}
		p := Price{Input: in * 1e6, Output: out * 1e6, CacheRead: cr * 1e6}
		idx.flat[m.ID] = p
		if i := strings.LastIndexByte(m.ID, '/'); i >= 0 && i < len(m.ID)-1 {
			suffix := m.ID[i+1:]
			if _, exists := idx.flat[suffix]; !exists {
				idx.flat[suffix] = p
			}
		}
	}
	return idx, nil
}

func priceFromCatalogCost(c *catalogCost) Price {
	p := Price{Input: c.Input, Output: c.Output, CacheRead: c.CacheRead}
	for _, t := range c.Tiers {
		if t.Tier.Size > 0 {
			p.Tiers = append(p.Tiers, PriceTier{
				Size: t.Tier.Size, Input: t.Input, Output: t.Output, CacheRead: t.CacheRead,
			})
		}
	}
	sort.Slice(p.Tiers, func(i, j int) bool { return p.Tiers[i].Size < p.Tiers[j].Size })
	return p
}

// applyIndexes 按源顺序为每个上游模型选价。
func (pb *PriceBook) applyIndexes(indexes []*sourceIndex, ups []UpstreamConfig) error {
	quotes := make(map[string]Quote, len(ups))
	var missing []string
	for _, up := range ups {
		q, ok := pb.resolveFrom(up, indexes)
		if !ok && pb.cfg.Fallback != nil && (pb.cfg.Fallback.Input > 0 || pb.cfg.Fallback.Output > 0) {
			q = Quote{
				Price: Price{
					Input:     pb.fromDisplay(pb.cfg.Fallback.Input),
					Output:    pb.fromDisplay(pb.cfg.Fallback.Output),
					CacheRead: pb.fromDisplay(pb.cfg.Fallback.CacheRead),
				},
				Provider: "fallback",
				Source:   "fallback",
			}
			ok = true
		}
		if ok {
			quotes[up.ModelName] = q
		} else {
			missing = append(missing, up.ModelName)
		}
	}

	names := make([]string, 0, len(indexes))
	for _, idx := range indexes {
		names = append(names, idx.name)
	}

	pb.mu.Lock()
	pb.quotes = quotes
	pb.missing = missing
	pb.sources = names
	pb.updated = time.Now()
	pb.mu.Unlock()

	for _, up := range ups {
		if q, ok := quotes[up.ModelName]; ok {
			pb.log.Info("模型定价",
				"model", up.ModelName,
				"provider", q.Provider,
				"source", q.Source,
				"input_per_1m", round6(pb.toDisplay(q.Price.Input)),
				"output_per_1m", round6(pb.toDisplay(q.Price.Output)),
				"currency", pb.cfg.Currency,
			)
			// 指定了 provider 却从别的索引命中，说明价格可能不是该厂商官方价，明确提示
			if up.Provider != "" && q.Provider == "" {
				pb.log.Warn("该模型在指定 provider 下没有价格，已回退到价格源的通用索引（可能不是官方价）",
					"model", up.ModelName, "provider", up.Provider, "source", q.Source)
			}
		}
	}
	for _, name := range missing {
		hint := "该模型不在任何价格源中：可配置 price_model 指向等价模型，或直接用 price 手动定价"
		for _, up := range ups {
			if up.ModelName == name && up.Provider != "" {
				hint = fmt.Sprintf("provider %q 下没有该模型：确认 provider 是否正确，或用 price_model / price 手动指定", up.Provider)
			}
		}
		pb.log.Warn("模型未定价，请求将被拒绝", "model", name, "hint", hint)
	}
	return nil
}

// apply 用缓存的单源响应重建单价表（离线兜底）。
func (pb *PriceBook) apply(raw []byte, ups []UpstreamConfig) error {
	idx, err := parseCatalog(raw, "cache")
	if err != nil {
		// 缓存也可能来自 openrouter
		idx2, err2 := parseOpenRouter(raw, "cache")
		if err2 != nil {
			return err
		}
		idx = idx2
	}
	return pb.applyIndexes([]*sourceIndex{idx}, ups)
}

func (pb *PriceBook) resolveFrom(up UpstreamConfig, indexes []*sourceIndex) (Quote, bool) {
	// 1) 手动定价优先
	if up.Price != nil {
		return Quote{
			Price: Price{
				Input:     pb.fromDisplay(up.Price.Input),
				Output:    pb.fromDisplay(up.Price.Output),
				CacheRead: pb.fromDisplay(up.Price.CacheRead),
			},
			Provider: "manual",
			Source:   "manual",
		}, true
	}

	model := strings.TrimSpace(up.PriceModel)
	if model == "" {
		model = up.ModelName
	}

	// 2) 按源顺序命中
	for _, idx := range indexes {
		if p, provider, ok := idx.lookup(up.Provider, model); ok {
			return Quote{Price: p, Provider: provider, Source: idx.name}, true
		}
	}
	return Quote{}, false
}

// StartAutoRefresh 定期刷新价格与汇率。
func (pb *PriceBook) StartAutoRefresh(ctx context.Context, ups []UpstreamConfig) {
	if !pb.cfg.Enabled {
		return
	}
	if pb.cfg.RefreshHours > 0 {
		go func() {
			ticker := time.NewTicker(time.Duration(pb.cfg.RefreshHours) * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := pb.Load(ctx, ups); err != nil {
						pb.log.Warn("刷新模型价格失败，继续沿用上次价格", "err", err)
					}
				}
			}
		}()
	}
	if pb.fx != nil {
		pb.fx.StartAutoRefresh(ctx)
	}
}

// ---------------------------------------------------------------------------
// 对外信息
// ---------------------------------------------------------------------------

// ModelRate 描述一个网关模型的 credits 消耗倍率。
// Input / Output / CacheRead 的单位是「credits / token」，数值上等于
// 模型美元单价 / 1M tokens（因为 1 credit = 0.000001 USD）。
type ModelRate struct {
	Model     string  `json:"model"`
	Provider  string  `json:"provider,omitempty"`
	Source    string  `json:"source,omitempty"`
	Input     float64 `json:"input"`
	Output    float64 `json:"output"`
	CacheRead float64 `json:"cache_read"`
}

// PricingInfo 是 /quota 与 /admin 里展示的价格元信息。
type PricingInfo struct {
	Enabled       bool        `json:"enabled"`
	Sources       []string    `json:"sources"`
	Currency      string      `json:"currency"`
	Symbol        string      `json:"symbol"`
	CreditUnitUSD float64     `json:"credit_unit_usd"`
	CreditUnit    float64     `json:"credit_unit"` // 1 credit 折合展示货币
	UpdatedAt     string      `json:"updated_at,omitempty"`
	Models        int         `json:"models"`
	Missing       []string    `json:"missing_models,omitempty"`
	LastError     string      `json:"last_error,omitempty"`
	FX            FXInfo      `json:"fx"`
	Rates         []ModelRate `json:"rates,omitempty"`
}

func (pb *PriceBook) Info() PricingInfo {
	pb.mu.RLock()
	defer pb.mu.RUnlock()
	info := PricingInfo{
		Enabled:       pb.cfg.Enabled,
		Sources:       append([]string(nil), pb.sources...),
		Currency:      pb.cfg.Currency,
		Symbol:        pb.Symbol(),
		CreditUnitUSD: creditUnitUSD,
		CreditUnit:    round8(pb.toDisplay(creditUnitUSD)),
		Models:        len(pb.quotes),
		Missing:       append([]string(nil), pb.missing...),
	}
	rates := make([]ModelRate, 0, len(pb.quotes))
	for model, q := range pb.quotes {
		rates = append(rates, ModelRate{
			Model:     model,
			Provider:  q.Provider,
			Source:    q.Source,
			Input:     round6(q.Price.Input),
			Output:    round6(q.Price.Output),
			CacheRead: round6(q.Price.CacheRead),
		})
	}
	sort.Slice(rates, func(i, j int) bool { return rates[i].Model < rates[j].Model })
	info.Rates = rates
	if pb.fx != nil {
		info.FX = pb.fx.Info()
	}
	if !pb.updated.IsZero() {
		info.UpdatedAt = pb.updated.Format(time.RFC3339)
	}
	if pb.lastErr != nil {
		info.LastError = pb.lastErr.Error()
	}
	return info
}

// effectiveSources 归一化价格源配置（兼容旧的单一 source_url）。
func (c PricingConfig) effectiveSources() []PriceSource {
	var out []PriceSource
	for _, s := range c.Sources {
		if s.URL == "" || !s.enabled() {
			continue
		}
		if s.Format == "" {
			s.Format = "catalog"
		}
		if s.Name == "" {
			if u, err := url.Parse(s.URL); err == nil {
				s.Name = u.Hostname()
			} else {
				s.Name = "source"
			}
		}
		out = append(out, s)
	}
	if len(out) == 0 && strings.TrimSpace(c.SourceURL) != "" {
		out = append(out, PriceSource{Name: "models5", URL: c.SourceURL, Format: "catalog"})
	}
	return out
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
func round8(v float64) float64 { return math.Round(v*1e8) / 1e8 }
