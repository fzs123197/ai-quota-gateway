// ai-quota-gateway
//
// 单用户自用的多协议 LLM 额度代理网关。
// 入口支持 OpenAI Chat Completions / OpenAI Responses / Anthropic Messages，
// 上游协议由 upstream[].protocol 指定，入口与上游不同时自动互转。
// 上游为按量计费的 LLM API（DeepSeek / 通义千问 / 智谱 GLM 等），
// 本网关负责：模型路由、三重额度校验（月 / 周 / 5 小时滑动窗口）、
// 预扣额度与多退少补、SSE 流式透传、额度面板。
//
// 设计约束（刻意为之）：
//   - 全部配置来自可执行文件同目录的 config.json，无环境变量 / 命令行覆盖，无热重载。
//   - 存储只用 Redis（开启 RDB 持久化），额度 key 自带 TTL。
//   - 单实例部署：进程内不缓存额度，所有计数以 Redis 为准。
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// 常量
// ---------------------------------------------------------------------------

const (
	// Redis key 过期策略（需求规定）
	monthKeyTTL  = 32 * 24 * time.Hour
	weekKeyTTL   = 8 * 24 * time.Hour
	windowKeyTTL = 6 * time.Hour

	// 5 小时滑动窗口
	windowSpan = 5 * time.Hour

	// 计价
	defaultPricingURL = "https://models5.com/catalog.json"
	defaultFXURL      = "https://open.er-api.com/v6/latest/USD"
	pricingCacheFile  = "pricing-cache.json"
	fxCacheFile       = "fx-cache.json"
	costScale         = 1e9 // 金额以「纳单位」整数存储，1 单位货币 = 1e9
	catalogMaxBytes   = 64 << 20

	maxBodyBytes           = 32 << 20 // 请求 / 响应体上限
	defaultUpstreamTimeout = 600 * time.Second
	reserveRetryLimit      = 8

	// token 预估系数：中文 1 字 ≈ 2 token，英文 4 字符 ≈ 1 token
	tokensPerCJKChar     = 2
	charsPerToken        = 4
	messageOverhead      = 4
	minCompletionReserve = 128

	// 请求状态（写入日志的 status 字段）
	statusAllowed       = "allowed"        // 放行并已按真实 usage 记账
	statusQuotaExceeded = "quota_exceeded" // 额度超限，未转发上游
	statusUpstreamError = "upstream_error" // 上游报错 / 超时 / 连接中断
	statusClientAbort   = "client_abort"   // 客户端主动断开
	statusNoUsage       = "no_usage"       // 上游成功但未返回 usage，不计费
	statusBadRequest    = "bad_request"    // 模型未配置 / 报文非法

	// 运行模式
	//   local   本地网关模式：只服务本机，强制监听回环地址，前台运行，控制台给出引导信息
	//   service 服务模式：监听配置中的地址（可对外），由 systemd 常驻托管
	ModeLocal   = "local"
	ModeService = "service"
)

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

type Config struct {
	Gateway  GatewayConfig    `json:"gateway"`
	Redis    RedisConfig      `json:"redis"`
	Quota    QuotaConfig      `json:"quota"`
	Pricing  PricingConfig    `json:"pricing"`
	Upstream []UpstreamConfig `json:"upstream"`
}

type GatewayConfig struct {
	Mode       string `json:"mode"`
	APIKey     string `json:"api_key"`   // 引导密钥：首次启动写入 Redis，之后以 Redis 里的 key 为准
	AdminKey   string `json:"admin_key"` // 管理台密钥，留空则沿用 api_key
	ListenAddr string `json:"listen_addr"`
}

// adminKey 返回管理台使用的密钥。
func (g GatewayConfig) adminKey() string {
	if strings.TrimSpace(g.AdminKey) != "" {
		return strings.TrimSpace(g.AdminKey)
	}
	return strings.TrimSpace(g.APIKey)
}

// mode 返回运行模式。未配置时默认 local（本机自用）。
func (g GatewayConfig) mode() string {
	m := strings.ToLower(strings.TrimSpace(g.Mode))
	if m == "" {
		return ModeLocal
	}
	return m
}

// isWildcardHost 判断监听地址是否绑定了所有网卡（":8080" / "0.0.0.0:8080" / "[::]:8080"）。
func isWildcardHost(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return false
	}
	return host == "" || host == "0.0.0.0" || host == "::"
}

// resolveListenAddr 按运行模式确定最终监听地址。
//
// 本地模式：只允许回环地址。配置为 ":8080" / "0.0.0.0:8080" / "[::]:8080" 时自动改绑
// 127.0.0.1；显式写了对外地址则直接拒绝启动，避免"以为是本地、实际暴露在公网"。
// 服务模式：原样使用配置地址。
func resolveListenAddr(g GatewayConfig) (string, error) {
	addr := strings.TrimSpace(g.ListenAddr)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("gateway.listen_addr 格式错误（应形如 127.0.0.1:8080 或 :8080）：%w", err)
	}
	if port == "" {
		return "", errors.New("gateway.listen_addr 缺少端口")
	}
	if g.mode() == ModeService {
		return addr, nil
	}

	switch host {
	case "", "0.0.0.0", "::":
		return net.JoinHostPort("127.0.0.1", port), nil
	case "localhost":
		return net.JoinHostPort("127.0.0.1", port), nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return addr, nil
	}
	return "", fmt.Errorf("本地模式只能监听回环地址，%q 会对外暴露；若确实要对外提供服务，请把 gateway.mode 改为 %q", addr, ModeService)
}

type RedisConfig struct {
	Addr     string `json:"addr"`
	Password string `json:"password"`
	DB       int    `json:"db"`
}

// QuotaConfig 是默认方案的三重 credits 上限（1 credit = 0.000001 USD）。
type QuotaConfig struct {
	Month          int64 `json:"month"`
	Week           int64 `json:"week"`
	FiveHourWindow int64 `json:"five_hour_window"`
}

// PricingConfig 控制模型价格来源与计费货币。
type PricingConfig struct {
	Enabled        bool          `json:"enabled"`
	SourceURL      string        `json:"source_url,omitempty"` // 兼容旧配置：等价于只配一个 catalog 源
	Sources        []PriceSource `json:"sources,omitempty"`    // 多价格源，按顺序优先
	Currency       string        `json:"currency"`             // USD / CNY
	RefreshHours   int           `json:"refresh_hours"`
	TimeoutSeconds int           `json:"timeout_seconds"`
	Fallback       *PriceInput   `json:"fallback,omitempty"`
	FX             FXConfig      `json:"fx"`
}

// PriceInput 是「每百万 token」的价格，单位与 pricing.currency 一致（手动配置时）。
// 从价格源拉取的价格固定为美元，展示时按实时汇率换算。
type PriceInput struct {
	Input     float64 `json:"input"`
	Output    float64 `json:"output"`
	CacheRead float64 `json:"cache_read"`
}

type UpstreamConfig struct {
	ModelName        string      `json:"model_name"`
	BaseURL          string      `json:"base_url"`
	APIKey           string      `json:"api_key"`
	TimeoutSeconds   int         `json:"timeout_seconds"`
	Protocol         string      `json:"protocol,omitempty"`          // chat（默认）/ responses / anthropic
	AnthropicVersion string      `json:"anthropic_version,omitempty"` // protocol=anthropic 时发送的 anthropic-version
	Provider         string      `json:"provider,omitempty"`          // models5 的 provider id，如 deepseek / alibaba-cn / zhipuai
	PriceModel       string      `json:"price_model,omitempty"`       // 价格目录里的模型 id（默认与 model_name 相同）
	Price            *PriceInput `json:"price,omitempty"`             // 手动指定价格，优先级最高
}

func (u UpstreamConfig) protocol() Protocol { return normalizeProtocol(u.Protocol) }

func (u UpstreamConfig) anthropicVersion() string {
	if v := strings.TrimSpace(u.AnthropicVersion); v != "" {
		return v
	}
	return "2023-06-01"
}

func (u UpstreamConfig) timeout() time.Duration {
	if u.TimeoutSeconds > 0 {
		return time.Duration(u.TimeoutSeconds) * time.Second
	}
	return defaultUpstreamTimeout
}

// configPath 返回 config.json 的绝对路径：优先可执行文件同目录，其次当前工作目录
// （后者仅用于 `go run` 这类二进制落在临时目录的场景）。
func configPath() string {
	if exe, err := os.Executable(); err == nil {
		if p, err := filepath.EvalSymlinks(exe); err == nil {
			exe = p
		}
		candidate := filepath.Join(filepath.Dir(exe), "config.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "config.json"
}

func loadConfig() (*Config, error) {
	path := configPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// applyDefaults 补齐可选配置的默认值（只补缺省，不覆盖用户显式配置）。
func (c *Config) applyDefaults() {
	if strings.TrimSpace(c.Pricing.Currency) == "" {
		c.Pricing.Currency = "USD"
	}
	c.Pricing.Currency = strings.ToUpper(strings.TrimSpace(c.Pricing.Currency))
	if c.Pricing.TimeoutSeconds <= 0 {
		c.Pricing.TimeoutSeconds = 60 // catalog 类价格源体积较大，国内网络留足时间
	}
	if len(c.Pricing.Sources) == 0 && strings.TrimSpace(c.Pricing.SourceURL) == "" && c.Pricing.Enabled {
		c.Pricing.Sources = []PriceSource{{Name: "models5", URL: defaultPricingURL, Format: "catalog"}}
	}
	if strings.TrimSpace(c.Pricing.FX.URL) == "" && c.Pricing.Enabled && c.Pricing.Currency == "CNY" {
		c.Pricing.FX.URL = defaultFXURL
	}
	if strings.TrimSpace(c.Pricing.FX.Format) == "" {
		c.Pricing.FX.Format = "rates"
	}
	if c.Pricing.FX.RefreshHours <= 0 {
		c.Pricing.FX.RefreshHours = 6
	}
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.Gateway.APIKey) == "" {
		return errors.New("gateway.api_key 不能为空")
	}
	switch c.Gateway.mode() {
	case ModeLocal, ModeService:
	default:
		return fmt.Errorf("gateway.mode 只能是 %q 或 %q，当前为 %q", ModeLocal, ModeService, c.Gateway.Mode)
	}
	if strings.TrimSpace(c.Gateway.ListenAddr) == "" {
		return errors.New("gateway.listen_addr 不能为空")
	}
	if _, err := resolveListenAddr(c.Gateway); err != nil {
		return err
	}
	if strings.TrimSpace(c.Redis.Addr) == "" {
		return errors.New("redis.addr 不能为空")
	}
	if c.Quota.Month <= 0 || c.Quota.Week <= 0 || c.Quota.FiveHourWindow <= 0 {
		return errors.New("quota.month / quota.week / quota.five_hour_window（credits）必须为正整数")
	}
	if !c.Pricing.Enabled {
		return errors.New("credits 计费需要 pricing.enabled=true（按模型价格自动计算消耗倍率）")
	}
	if c.Pricing.Enabled {
		for i, s := range c.Pricing.Sources {
			if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
				return fmt.Errorf("pricing.sources[%d].url 必须以 http:// 或 https:// 开头", i)
			}
			switch strings.ToLower(s.Format) {
			case "", "catalog", "modelsdev", "openrouter":
			default:
				return fmt.Errorf("pricing.sources[%d].format 只能是 catalog / modelsdev / openrouter，当前为 %q", i, s.Format)
			}
		}
		if len(c.Pricing.Sources) == 0 && c.Pricing.SourceURL == "" {
			return errors.New("pricing.enabled=true 时必须配置 pricing.sources（或兼容字段 source_url）")
		}
		switch c.Pricing.Currency {
		case "USD":
		case "CNY":
			if strings.TrimSpace(c.Pricing.FX.URL) == "" && c.Pricing.FX.FallbackRate <= 0 {
				return errors.New("pricing.currency=CNY 时必须配置 pricing.fx.url（实时汇率）或 pricing.fx.fallback_rate")
			}
			if c.Pricing.FX.FallbackRate < 0 {
				return errors.New("pricing.fx.fallback_rate 不能为负数")
			}
		default:
			return fmt.Errorf("pricing.currency 只能是 USD 或 CNY，当前为 %q", c.Pricing.Currency)
		}
		if c.Pricing.RefreshHours < 0 {
			return errors.New("pricing.refresh_hours 不能为负数（0 表示只在启动时拉取一次）")
		}
	}
	if len(c.Upstream) == 0 {
		return errors.New("upstream 列表不能为空")
	}
	seen := make(map[string]bool, len(c.Upstream))
	for i, u := range c.Upstream {
		if strings.TrimSpace(u.ModelName) == "" {
			return fmt.Errorf("upstream[%d].model_name 不能为空", i)
		}
		if strings.TrimSpace(u.BaseURL) == "" {
			return fmt.Errorf("upstream[%d].base_url 不能为空", i)
		}
		if !strings.HasPrefix(u.BaseURL, "http://") && !strings.HasPrefix(u.BaseURL, "https://") {
			return fmt.Errorf("upstream[%d].base_url 必须以 http:// 或 https:// 开头", i)
		}
		if strings.TrimSpace(u.APIKey) == "" {
			return fmt.Errorf("upstream[%d].api_key 不能为空", i)
		}
		if p := normalizeProtocol(u.Protocol); !protocolValid(p) {
			return fmt.Errorf("upstream[%d].protocol 只能是 chat / responses / anthropic，当前为 %q", i, u.Protocol)
		}
		if seen[u.ModelName] {
			return fmt.Errorf("upstream model_name %q 重复", u.ModelName)
		}
		seen[u.ModelName] = true
	}
	return nil
}

// ---------------------------------------------------------------------------
// 额度管理（Redis）
// ---------------------------------------------------------------------------

// QuotaExceededError 表示三重 credits 校验中某一项超限。
// Scope 为 month / week / five_hour。
type QuotaExceededError struct {
	Scope string
	Used  int64
	Limit int64
	Need  int64
}

func (e *QuotaExceededError) Error() string {
	return fmt.Sprintf("%s quota exceeded: used=%d need=%d limit=%d credits", e.Scope, e.Used, e.Need, e.Limit)
}

// Reservation 是一次请求的预扣凭据（单位：credits）。Settle / Rollback 二选一，且只会生效一次。
type Reservation struct {
	keyID    string
	reqID    string
	credits  int64
	score    int64 // 进入 5h 窗口的时间戳（毫秒），结算时保持不变
	member   string
	monthKey string
	weekKey  string
	done     atomic.Bool
}

// QuotaManager 只负责读写 credits 计数，限额由调用方按 key 当前绑定的 Plan 传入，
// 这样切换方案后立刻生效，无需重启、也不影响已累计的用量。
type QuotaManager struct {
	rdb *redis.Client
}

func NewQuotaManager(rdb *redis.Client) *QuotaManager { return &QuotaManager{rdb: rdb} }

// quotaKeys 返回某个 key 在指定时间点的 credits 额度 key（按 keyID 隔离）。
// 使用 credits: 前缀，避免与旧版 token / 金额计数混用。
func quotaKeys(keyID string, t time.Time) (month, week string) {
	prefix := "quota:" + keyID + ":credits:"
	y, w := t.ISOWeek()
	return prefix + "month:" + t.Format("2006-01"),
		fmt.Sprintf("%sweek:%04d-W%02d", prefix, y, w)
}

func windowKeyOf(keyID string) string { return "quota:" + keyID + ":credits:5h" }

// 5h 窗口成员格式：reqID|credits。
func memberName(reqID string, credits int64) string {
	return reqID + "|" + strconv.FormatInt(credits, 10)
}

func parseMember(member string) int64 {
	parts := strings.Split(member, "|")
	if len(parts) >= 2 {
		v, _ := strconv.ParseInt(parts[1], 10, 64)
		if v > 0 {
			return v
		}
	}
	return 0
}

// Reserve 执行「三重校验 + 预扣 credits」。
//
// 原子性：用 WATCH 监听所有额度 key，在事务内读用量、判额度、写预扣（MULTI/EXEC 由
// TxPipelined 打包），一旦有并发写入则 EXEC 失败并自动重试。整个「读-判-写」不会
// 被其它请求穿插；读与写各自只用一次 Pipeline 往返。
//
// 5h 窗口是真正的滑动窗口：ZSet 的 score 是请求进入时刻（毫秒），每次预扣前先
// ZREMRANGEBYSCORE 剔除 5 小时之前的成员，窗口边界随时间连续滑动，不是固定时间桶。
func (q *QuotaManager) Reserve(ctx context.Context, keyID string, plan Plan, reqID string, credits int64) (*Reservation, error) {
	if credits < 1 {
		credits = 1
	}
	limMonth, limWeek, limFive := plan.Month, plan.Week, plan.FiveHour

	now := time.Now()
	cutoff := now.Add(-windowSpan).UnixMilli()
	score := now.UnixMilli()
	mk, wk := quotaKeys(keyID, now)
	windowKey := windowKeyOf(keyID)
	member := memberName(reqID, credits)

	for attempt := 0; attempt < reserveRetryLimit; attempt++ {
		var res *Reservation
		err := q.rdb.Watch(ctx, func(tx *redis.Tx) error {
			// 1) 滑动窗口修剪：移除 5 小时之前的成员
			if err := tx.ZRemRangeByScore(ctx, windowKey, "0", strconv.FormatInt(cutoff, 10)).Err(); err != nil {
				return err
			}

			// 2) 读取当前用量（Pipeline 单次往返）
			pipe := tx.Pipeline()
			monthCmd := pipe.Get(ctx, mk)
			weekCmd := pipe.Get(ctx, wk)
			windowCmd := pipe.ZRangeWithScores(ctx, windowKey, 0, -1)
			if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
				return err
			}
			monthUsed, weekUsed := cmdInt64(monthCmd), cmdInt64(weekCmd)
			var fiveUsed int64
			for _, z := range windowCmd.Val() {
				fiveUsed += parseMember(fmt.Sprint(z.Member))
			}

			// 3) 三重校验：任意一项超限即拒绝
			if monthUsed+credits > limMonth {
				return &QuotaExceededError{Scope: "month", Used: monthUsed, Limit: limMonth, Need: credits}
			}
			if weekUsed+credits > limWeek {
				return &QuotaExceededError{Scope: "week", Used: weekUsed, Limit: limWeek, Need: credits}
			}
			if fiveUsed+credits > limFive {
				return &QuotaExceededError{Scope: "five_hour", Used: fiveUsed, Limit: limFive, Need: credits}
			}

			// 4) 预扣（Pipeline + MULTI/EXEC，单次往返写入并续期）
			if _, err := tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
				p.IncrBy(ctx, mk, credits)
				p.IncrBy(ctx, wk, credits)
				p.Expire(ctx, mk, monthKeyTTL)
				p.Expire(ctx, wk, weekKeyTTL)
				p.ZAdd(ctx, windowKey, redis.Z{Score: float64(score), Member: member})
				p.Expire(ctx, windowKey, windowKeyTTL)
				return nil
			}); err != nil {
				return err
			}

			res = &Reservation{
				keyID:    keyID,
				reqID:    reqID,
				credits:  credits,
				score:    score,
				member:   member,
				monthKey: mk,
				weekKey:  wk,
			}
			return nil
		}, mk, wk, windowKey)

		if err == nil {
			return res, nil
		}
		var qe *QuotaExceededError
		if errors.As(err, &qe) {
			return nil, qe
		}
		if errors.Is(err, redis.TxFailedErr) {
			continue // 乐观锁冲突，重试
		}
		return nil, err
	}
	return nil, errors.New("quota reservation conflict: too many concurrent writers")
}

// Settle 用真实 credits 结算：多退少补。返回本次 credits 变化（delta）。
func (q *QuotaManager) Settle(ctx context.Context, r *Reservation, actualCredits int64) (delta int64) {
	if r == nil || !r.done.CompareAndSwap(false, true) {
		return 0
	}
	if actualCredits < 0 {
		actualCredits = 0
	}
	delta = actualCredits - r.credits

	pipe := q.rdb.Pipeline()
	if delta != 0 {
		pipe.IncrBy(ctx, r.monthKey, delta)
		pipe.IncrBy(ctx, r.weekKey, delta)
	}
	pipe.ZRem(ctx, windowKeyOf(r.keyID), r.member)
	if actualCredits > 0 {
		pipe.ZAdd(ctx, windowKeyOf(r.keyID), redis.Z{
			Score:  float64(r.score),
			Member: memberName(r.reqID, actualCredits),
		})
	}
	pipe.Expire(ctx, r.monthKey, monthKeyTTL)
	pipe.Expire(ctx, r.weekKey, weekKeyTTL)
	pipe.Expire(ctx, windowKeyOf(r.keyID), windowKeyTTL)
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		slog.Error("settle quota failed", "req_id", r.reqID, "err", err)
	}
	q.clampNonNegative(ctx, r.monthKey, r.weekKey)
	return delta
}

// Rollback 回滚预扣（上游失败 / 超时 / 客户端断开 / 未拿到 usage）。
func (q *QuotaManager) Rollback(ctx context.Context, r *Reservation) {
	if r == nil || !r.done.CompareAndSwap(false, true) {
		return
	}
	pipe := q.rdb.Pipeline()
	pipe.DecrBy(ctx, r.monthKey, r.credits)
	pipe.DecrBy(ctx, r.weekKey, r.credits)
	pipe.ZRem(ctx, windowKeyOf(r.keyID), r.member)
	pipe.Expire(ctx, r.monthKey, monthKeyTTL)
	pipe.Expire(ctx, r.weekKey, weekKeyTTL)
	pipe.Expire(ctx, windowKeyOf(r.keyID), windowKeyTTL)
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		slog.Error("rollback quota failed", "req_id", r.reqID, "err", err)
	}
	q.clampNonNegative(ctx, r.monthKey, r.weekKey)
}

// clampNonNegative 兜底把计数器钳制到 0，避免异常场景下出现负额度（等于白送配额）。
func (q *QuotaManager) clampNonNegative(ctx context.Context, keys ...string) {
	pipe := q.rdb.Pipeline()
	cmds := make([]*redis.StringCmd, len(keys))
	ttls := make([]*redis.DurationCmd, len(keys))
	for i, k := range keys {
		cmds[i] = pipe.Get(ctx, k)
		ttls[i] = pipe.TTL(ctx, k)
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return
	}
	fix := q.rdb.Pipeline()
	dirty := false
	for i, c := range cmds {
		if v, err := c.Int64(); err == nil && v < 0 {
			ttl := ttls[i].Val()
			if ttl < 0 {
				ttl = 0
			}
			fix.Set(ctx, keys[i], 0, ttl)
			dirty = true
		}
	}
	if dirty {
		_, _ = fix.Exec(ctx)
	}
}

// Snapshot 是一次额度快照（单位：credits）。
type Snapshot struct {
	MonthUsed, WeekUsed, FiveUsed    int64
	MonthLimit, WeekLimit, FiveLimit int64
}

func (q *QuotaManager) Snapshot(ctx context.Context, keyID string, plan Plan) (*Snapshot, error) {
	now := time.Now()
	cutoff := now.Add(-windowSpan).UnixMilli()
	mk, wk := quotaKeys(keyID, now)
	windowKey := windowKeyOf(keyID)

	pipe := q.rdb.Pipeline()
	monthCmd := pipe.Get(ctx, mk)
	weekCmd := pipe.Get(ctx, wk)
	windowCmd := pipe.ZRangeByScoreWithScores(ctx, windowKey, &redis.ZRangeBy{
		Min: strconv.FormatInt(cutoff, 10),
		Max: "+inf",
	})
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	var five int64
	for _, z := range windowCmd.Val() {
		five += parseMember(fmt.Sprint(z.Member))
	}
	return &Snapshot{
		MonthUsed: cmdInt64(monthCmd), WeekUsed: cmdInt64(weekCmd), FiveUsed: five,
		MonthLimit: plan.Month, WeekLimit: plan.Week, FiveLimit: plan.FiveHour,
	}, nil
}

func cmdInt64(cmd *redis.StringCmd) int64 {
	v, err := cmd.Int64()
	if err != nil {
		return 0
	}
	return v
}

// ---- 金额工具：内部一律用「纳单位」整数，避免浮点累计误差 ----

func moneyToNano(v float64) int64 { return int64(math.Round(v * costScale)) }
func nanoToMoney(n int64) float64 { return float64(n) / costScale }

// moneyStr 用于日志：固定 8 位小数，避免出现 4.2e-05 这种科学计数法。
func moneyStr(n int64) string { return strconv.FormatFloat(nanoToMoney(n), 'f', 8, 64) }

// ---------------------------------------------------------------------------
// OpenAI 协议结构
// ---------------------------------------------------------------------------

type promptTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type completionTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	// 缓存命中：DeepSeek 用 prompt_cache_hit_tokens，OpenAI 用 prompt_tokens_details.cached_tokens
	PromptCacheHitTokens    int64                    `json:"prompt_cache_hit_tokens,omitempty"`
	PromptTokensDetails     *promptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *completionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// cacheHitTokens 返回命中缓存的输入 token 数（两种协议取其一）。
func (u *Usage) cacheHitTokens() int64 {
	if u == nil {
		return 0
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		hit := u.PromptTokensDetails.CachedTokens
		if hit > u.PromptTokens {
			hit = u.PromptTokens
		}
		return hit
	}
	if u.PromptCacheHitTokens > 0 && u.PromptCacheHitTokens <= u.PromptTokens {
		return u.PromptCacheHitTokens
	}
	return 0
}

func (u *Usage) total() int64 {
	if u == nil {
		return 0
	}
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.PromptTokens + u.CompletionTokens
}

// ---------------------------------------------------------------------------
// token 预估
// ---------------------------------------------------------------------------

func countText(s string) int64 {
	var cjk, other int64
	for _, r := range s {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	return cjk*tokensPerCJKChar + (other+charsPerToken-1)/charsPerToken
}

func isCJK(r rune) bool {
	return (r >= 0x3000 && r <= 0x303F) || // CJK 标点
		(r >= 0x3040 && r <= 0x30FF) || // 日文假名
		(r >= 0x3400 && r <= 0x4DBF) || // 扩展 A
		(r >= 0x4E00 && r <= 0x9FFF) || // 基本汉字
		(r >= 0xAC00 && r <= 0xD7AF) || // 谚文
		(r >= 0xF900 && r <= 0xFAFF) || // 兼容汉字
		(r >= 0xFF00 && r <= 0xFFEF) // 全角字符
}

// tokenEstimate 是预扣用的 token 拆分（金额预扣需要分别乘输入/输出单价）。
type tokenEstimate struct {
	Prompt     int64
	Completion int64
}

func (e tokenEstimate) Total() int64 { return e.Prompt + e.Completion }

// ensureIncludeUsage 在流式请求中强制打开 stream_options.include_usage，
// 否则上游不会在最后一个 chunk 里返回 usage，也就无法按真实用量记账。
func ensureIncludeUsage(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	opts := map[string]json.RawMessage{}
	if raw, ok := m["stream_options"]; ok && len(raw) > 0 && string(raw) != "null" {
		_ = json.Unmarshal(raw, &opts)
	}
	opts["include_usage"] = json.RawMessage("true")
	optsRaw, err := json.Marshal(opts)
	if err != nil {
		return body
	}
	m["stream_options"] = optsRaw
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// ---------------------------------------------------------------------------
// 网关服务
// ---------------------------------------------------------------------------

type Server struct {
	cfg       *Config
	qm        *QuotaManager
	store     *Store
	prices    *PriceBook
	client    *http.Client
	log       *slog.Logger
	upstreams map[string]*UpstreamConfig
	models    []string
	indexPath string
	adminPath string
}

func NewServer(cfg *Config, rdb *redis.Client, prices *PriceBook, logger *slog.Logger) *Server {
	dir := filepath.Dir(configPath())
	s := &Server{
		cfg:       cfg,
		qm:        NewQuotaManager(rdb),
		store:     NewStore(rdb),
		prices:    prices,
		log:       logger,
		upstreams: make(map[string]*UpstreamConfig, len(cfg.Upstream)),
		indexPath: filepath.Join(dir, "index.html"),
		adminPath: filepath.Join(dir, "admin.html"),
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   20,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 120 * time.Second,
				ExpectContinueTimeout: time.Second,
				ForceAttemptHTTP2:     true,
			},
			// 不设 Client.Timeout：超时由每个请求的 context 控制，流式响应才能长时间存活
		},
	}
	for i := range cfg.Upstream {
		u := &cfg.Upstream[i]
		s.upstreams[u.ModelName] = u
		s.models = append(s.models, u.ModelName)
	}
	return s
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.auth(s.handleChat))
	mux.HandleFunc("/v1/responses", s.auth(s.handleResponses))
	mux.HandleFunc("/v1/messages", s.auth(s.handleMessages))
	mux.HandleFunc("/v1/messages/count_tokens", s.auth(s.handleCountTokens))
	mux.HandleFunc("/v1/models", s.auth(s.handleModels))
	mux.HandleFunc("/quota", s.auth(s.handleQuota))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok")
	})
	s.registerAdmin(mux)
	mux.HandleFunc("/", s.auth(s.handleIndex))
	return mux
}

// authInfo 是鉴权通过后挂在 context 上的信息。
type authInfo struct {
	Key  *APIKey
	Plan Plan
}

type authCtxKey struct{}

func authFrom(ctx context.Context) *authInfo {
	v, _ := ctx.Value(authCtxKey{}).(*authInfo)
	return v
}

// extractSecret 依次从 Authorization / x-api-key / ?key= / Cookie 里取凭据。
// x-api-key 用于兼容 Anthropic 客户端。
func extractSecret(r *http.Request, cookieName string) (secret string, fromQuery bool) {
	if s := bearerToken(r); s != "" {
		return s, false
	}
	if s := strings.TrimSpace(r.Header.Get("x-api-key")); s != "" {
		return s, false
	}
	if q := r.URL.Query().Get("key"); q != "" {
		return q, true
	}
	if c, err := r.Cookie(cookieName); err == nil {
		return c.Value, false
	}
	return "", false
}

// auth 网关鉴权：任何在 Redis 里登记且启用的 key 都可用。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret, fromQuery := extractSecret(r, "gw_key")
		if secret == "" {
			s.unauthorized(w, r, "缺少网关密钥", "gw_key")
			return
		}
		k, err := s.store.GetKeyBySecret(r.Context(), secret)
		if err != nil {
			if errors.Is(err, ErrKeyDisabled) {
				writeOpenAIError(w, http.StatusForbidden, "permission_denied", "该网关密钥已被停用")
				return
			}
			s.unauthorized(w, r, "网关密钥无效", "gw_key")
			return
		}
		plan, err := s.store.GetPlan(r.Context(), k.PlanID)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "api_error",
				fmt.Sprintf("该 key 绑定的方案 %q 不存在，请到管理台重新分配", k.PlanID))
			return
		}
		if fromQuery {
			http.SetCookie(w, &http.Cookie{
				Name: "gw_key", Value: secret, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 7 * 24 * 3600,
			})
		}
		ctx := context.WithValue(r.Context(), authCtxKey{}, &authInfo{Key: k, Plan: *plan})
		next(w, r.WithContext(ctx))
	}
}

// adminAuth 管理台鉴权：使用 config 里的 admin_key（留空则用 api_key）。
func (s *Server) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := s.cfg.Gateway.adminKey()
		secret, fromQuery := extractSecret(r, "gw_admin")
		if secret == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(want)) != 1 {
			s.unauthorized(w, r, "管理台密钥无效", "gw_admin")
			return
		}
		if fromQuery {
			http.SetCookie(w, &http.Cookie{
				Name: "gw_admin", Value: secret, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 7 * 24 * 3600,
			})
		}
		next(w, r)
	}
}

// unauthorized 对页面请求返回一个带提示的 401 HTML，对 API 返回 JSON。
func (s *Server) unauthorized(w http.ResponseWriter, r *http.Request, msg, cookieName string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="ai-quota-gateway"`)
	if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/v1/") && !strings.Contains(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>401</title>
<body style="font-family:system-ui;padding:48px;color:#222;line-height:1.7">
<h2>401 Unauthorized</h2>
<p>`+msg+`。请以 <code>?key=你的密钥</code> 访问一次，浏览器会记住凭据。</p>
</body>`)
		return
	}
	writeProtocolError(w, requestProtocol(r), http.StatusUnauthorized, "invalid_request_error", msg)
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// ---------------------------------------------------------------------------
// 业务 handler
// ---------------------------------------------------------------------------

// quotaResponse 是 GET /quota 的返回结构。用量与限额均为 credits。
type quotaResponse struct {
	Key           keyView     `json:"key"`
	MonthUsed     int64       `json:"month_used"`
	MonthQuota    int64       `json:"month_quota"`
	WeekUsed      int64       `json:"week_used"`
	WeekQuota     int64       `json:"week_quota"`
	FiveHourUsed  int64       `json:"five_hour_used"`
	FiveHourQuota int64       `json:"five_hour_quota"`
	Money         moneyView   `json:"money"`
	Pricing       PricingInfo `json:"pricing"`
	UpdatedAt     string      `json:"updated_at"`
}

// keyView 描述当前调用方使用的 key 及其方案。
type keyView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Prefix   string `json:"prefix"`
	PlanID   string `json:"plan_id"`
	PlanName string `json:"plan_name"`
}

// moneyView 是 credits 折算成展示货币后的等值金额。
type moneyView struct {
	Available     bool    `json:"available"`
	Currency      string  `json:"currency"`
	Symbol        string  `json:"symbol"`
	MonthUsed     float64 `json:"month_used"`
	MonthLimit    float64 `json:"month_limit"`
	WeekUsed      float64 `json:"week_used"`
	WeekLimit     float64 `json:"week_limit"`
	FiveHourUsed  float64 `json:"five_hour_used"`
	FiveHourLimit float64 `json:"five_hour_limit"`
}

func (s *Server) handleQuota(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	ai := authFrom(r.Context())
	if ai == nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", "unauthorized")
		return
	}
	snap, err := s.qm.Snapshot(r.Context(), ai.Key.ID, ai.Plan)
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "api_error", "redis unavailable: "+err.Error())
		return
	}
	creditUnit := s.prices.CreditUnit()
	moneyOf := func(credits int64) float64 { return round8(float64(credits) * creditUnit) }
	resp := quotaResponse{
		Key: keyView{
			ID: ai.Key.ID, Name: ai.Key.Name, Prefix: ai.Key.Prefix,
			PlanID: ai.Plan.ID, PlanName: ai.Plan.Name,
		},
		MonthUsed:     snap.MonthUsed,
		MonthQuota:    snap.MonthLimit,
		WeekUsed:      snap.WeekUsed,
		WeekQuota:     snap.WeekLimit,
		FiveHourUsed:  snap.FiveUsed,
		FiveHourQuota: snap.FiveLimit,
		Money: moneyView{
			Available:     creditUnit > 0,
			Currency:      s.cfg.Pricing.Currency,
			Symbol:        s.prices.Symbol(),
			MonthUsed:     moneyOf(snap.MonthUsed),
			MonthLimit:    moneyOf(snap.MonthLimit),
			WeekUsed:      moneyOf(snap.WeekUsed),
			WeekLimit:     moneyOf(snap.WeekLimit),
			FiveHourUsed:  moneyOf(snap.FiveUsed),
			FiveHourLimit: moneyOf(snap.FiveLimit),
		},
		Pricing:   s.prices.Info(),
		UpdatedAt: time.Now().Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if requestProtocol(r) == protoAnthropic {
		type anthModel struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			CreatedAt   int64  `json:"created_at"`
			Type        string `json:"type"`
		}
		data := make([]anthModel, 0, len(s.models))
		for _, m := range s.models {
			data = append(data, anthModel{ID: m, DisplayName: m, CreatedAt: 0, Type: "model"})
		}
		out := map[string]any{"data": data, "has_more": false}
		if len(data) > 0 {
			out["first_id"] = data[0].ID
			out["last_id"] = data[len(data)-1].ID
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	type modelObj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	out := struct {
		Object string     `json:"object"`
		Data   []modelObj `json:"data"`
	}{Object: "list"}
	for _, m := range s.models {
		out.Data = append(out.Data, modelObj{ID: m, Object: "model", Created: 0, OwnedBy: "ai-quota-gateway"})
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	data, err := os.ReadFile(s.indexPath)
	if err != nil {
		http.Error(w, "index.html not found next to the binary: "+s.indexPath, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// requestLog 汇总一条结构化日志的全部字段。
type requestLog struct {
	ReqID          string
	KeyID          string
	KeyName        string
	Model          string
	ClientProto    string
	UpstreamProto  string
	Upstream       string
	Stream         bool
	EstTokens      int64
	Reserved       int64 // 预扣 credits
	Priced         bool  // 是否查到单价
	Usage          *Usage
	Credits        int64 // 本次真实消耗 credits
	Delta          int64 // credits 多退少补
	Cost           int64 // 本次真实花费（展示货币，纳单位，仅用于日志/统计）
	MonthUsed      int64
	WeekUsed       int64
	FiveHourUsed   int64
	MonthQuota     int64
	WeekQuota      int64
	FiveHourQuota  int64
	Status         string
	Detail         string
	UpstreamStatus int
	Latency        time.Duration
}

func (s *Server) emit(l *requestLog) {
	attrs := []any{
		"req_id", l.ReqID,
		"key", l.KeyName,
		"key_id", l.KeyID,
		"model", l.Model,
		"client_proto", l.ClientProto,
		"upstream_proto", l.UpstreamProto,
		"upstream", l.Upstream,
		"stream", l.Stream,
		"est_tokens", l.EstTokens,
		"credits_reserved", l.Reserved,
	}
	if l.Usage != nil {
		attrs = append(attrs,
			"usage_prompt", l.Usage.PromptTokens,
			"usage_completion", l.Usage.CompletionTokens,
			"usage_total", l.Usage.total(),
		)
	} else {
		attrs = append(attrs, "usage_total", int64(0))
	}
	attrs = append(attrs,
		"credits_used", l.Credits,
		"credits_delta", l.Delta,
		"credits_month_used", l.MonthUsed, "credits_month_quota", l.MonthQuota,
		"credits_week_used", l.WeekUsed, "credits_week_quota", l.WeekQuota,
		"credits_five_hour_used", l.FiveHourUsed, "credits_five_hour_quota", l.FiveHourQuota,
	)
	// 实际花费（展示货币），仅用于日志/统计
	if s.cfg.Pricing.Enabled {
		attrs = append(attrs,
			"priced", l.Priced,
			"currency", s.cfg.Pricing.Currency,
			"cost", moneyStr(l.Cost),
		)
	}
	attrs = append(attrs,
		"status", l.Status,
		"latency_ms", l.Latency.Milliseconds(),
	)
	if l.UpstreamStatus > 0 {
		attrs = append(attrs, "upstream_status", l.UpstreamStatus)
	}
	if l.Detail != "" {
		attrs = append(attrs, "detail", l.Detail)
	}
	if l.Status == statusAllowed {
		s.log.Info("gateway request", attrs...)
	} else {
		s.log.Warn("gateway request", attrs...)
	}
}

func (s *Server) fillUsage(ctx context.Context, keyID string, plan Plan, lg *requestLog) {
	snap, err := s.qm.Snapshot(ctx, keyID, plan)
	if err != nil {
		return
	}
	lg.MonthUsed = snap.MonthUsed
	lg.WeekUsed = snap.WeekUsed
	lg.FiveHourUsed = snap.FiveUsed
}

// printLocalBanner 在本地模式下打印一份可读的启动引导。
// 服务模式不打这个（日志直接进 journald，保持纯结构化输出）。
func (s *Server) printLocalBanner(addr string) {
	base := "http://" + addr
	key := s.cfg.Gateway.APIKey
	line := strings.Repeat("─", 54)
	fmt.Printf("\n  ai-quota-gateway · 本地网关模式（仅监听回环地址）\n  %s\n", line)
	fmt.Printf("  额度面板   %s/?key=%s\n", base, url.QueryEscape(key))
	fmt.Printf("  客户端     Base URL  %s/v1\n", base)
	fmt.Printf("             API Key   %s\n", key)
	fmt.Printf("  上游模型   %s\n", strings.Join(s.models, ", "))
	fmt.Printf("  接口       /v1/chat/completions · /v1/responses · /v1/messages\n")
	fmt.Printf("  管理台     %s/admin?key=%s\n", base, url.QueryEscape(s.cfg.Gateway.adminKey()))
	if s.cfg.Pricing.Enabled {
		sym := s.prices.Symbol()
		info := s.prices.Info()
		payLine := fmt.Sprintf("  计费       %s（%s）", s.cfg.Pricing.Currency, sym)
		if info.Models > 0 {
			payLine += fmt.Sprintf("｜已定价 %d 个模型", info.Models)
		}
		if len(info.Missing) > 0 {
			payLine += fmt.Sprintf("｜未定价 %d 个", len(info.Missing))
		}
		fmt.Println(payLine)
		fmt.Printf("  额度单位   credits（1 credit = $0.000001，模型按价格自动折算倍率）\n")
		if info.FX.Rate > 0 {
			fmt.Printf("  汇率       USD→CNY %.4f（%s）\n", info.FX.Rate, info.FX.Source)
		}
	}
	fmt.Printf("  存储       Redis %s (db=%d)\n", s.cfg.Redis.Addr, s.cfg.Redis.DB)
	fmt.Printf("  停止       Ctrl+C\n  %s\n\n", line)
}

// ---------------------------------------------------------------------------
// 工具函数
// ---------------------------------------------------------------------------

// accountingCtx 用于记账：客户端断开后 r.Context() 已取消，必须换一个独立 context。
func accountingCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    code,
			"code":    code,
		},
	})
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				a.Value = slog.StringValue(a.Value.Time().Format("2006-01-02 15:04:05.000"))
			}
			return a
		},
	}))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("config load failed", "err", err)
		os.Exit(1)
	}
	mode := cfg.Gateway.mode()

	listenAddr, err := resolveListenAddr(cfg.Gateway)
	if err != nil {
		logger.Error("config load failed", "err", err)
		os.Exit(1)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = rdb.Ping(pingCtx).Err()
	pingCancel()
	if err != nil {
		logger.Error("redis connect failed",
			"addr", cfg.Redis.Addr,
			"err", err,
			"hint", "额度计数依赖 Redis：确认服务已启动、redis.addr / redis.password 正确；Windows 本机可用 Memurai，或用 WSL / Docker 起一个 redis:7",
		)
		os.Exit(1)
	}

	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	// 引导：首次启动把 config 里的 api_key / quota 落成默认 key 与方案，
	// 并补齐 Go / Plus / Pro / Max 10x / Max 20x 内置预设。
	store := NewStore(rdb)
	bootCtx, bootCancel := context.WithTimeout(rootCtx, 10*time.Second)
	createdKey, planChanges, err := store.Bootstrap(bootCtx, cfg)
	bootCancel()
	if err != nil {
		logger.Error("初始化默认密钥/方案失败", "err", err)
		os.Exit(1)
	}
	if createdKey {
		logger.Info("已用 config.gateway.api_key 创建默认密钥（后续请在管理台维护）")
	}
	if planChanges > 0 {
		logger.Info("已创建/升级内置 Plan 方案", "count", planChanges, "presets", "Go, Plus, Pro, Max 10x, Max 20x")
	}

	prices := NewPriceBook(cfg.Pricing, logger)
	if cfg.Pricing.Enabled {
		// 汇率与价格都先读缓存秒开，再后台刷新
		if cfg.Pricing.Currency == "CNY" && prices.fx != nil {
			if rate, err := readFXCcache(prices.fx.cachePath()); err == nil && rate > 0 {
				prices.fx.set(rate, "cache", nil)
			}
		}
		if prices.LoadCache(cfg.Upstream) {
			logger.Info("已从本地缓存加载模型价格", "cache", prices.cachePath())
		}
		go func() {
			if prices.fx != nil {
				_ = prices.fx.Load(rootCtx)
			}
			if err := prices.Load(rootCtx, cfg.Upstream); err != nil {
				logger.Warn("加载模型价格失败，暂时按 0 计费（不影响转发与 token 额度）", "err", err)
			}
		}()
	}

	srv := NewServer(cfg, rdb, prices, logger)
	httpSrv := &http.Server{
		Addr:              listenAddr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 15 * time.Second,
		// 不设 WriteTimeout：SSE 长连接会被它掐断
	}

	logger.Info("ai-quota-gateway started",
		"mode", mode,
		"listen", listenAddr,
		"redis", cfg.Redis.Addr,
		"models", strings.Join(srv.models, ","),
		"pricing", cfg.Pricing.Enabled,
		"currency", cfg.Pricing.Currency,
		"credit_unit_usd", creditUnitUSD,
	)
	prices.StartAutoRefresh(rootCtx, cfg.Upstream)
	if mode == ModeLocal {
		if isWildcardHost(cfg.Gateway.ListenAddr) {
			logger.Warn("本地模式已把监听地址改绑到回环地址",
				"configured", cfg.Gateway.ListenAddr, "actual", listenAddr)
		}
		// systemd 会给服务进程注入 INVOCATION_ID；仅用于诊断提示，不影响配置优先级。
		if os.Getenv("INVOCATION_ID") != "" {
			logger.Warn("检测到 systemd 托管，但 gateway.mode=local：当前只监听回环地址，外部客户端无法访问；" +
				"若要在服务器上对外提供服务，请把 config.json 的 gateway.mode 改为 \"service\"")
		}
		srv.printLocalBanner(listenAddr)
	}

	done := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		logger.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		close(done)
	}()

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
	<-done
	_ = rdb.Close()
	if mode == ModeLocal {
		fmt.Println("\n  网关已停止。")
	}
}
