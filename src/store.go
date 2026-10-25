package main

// 存储层：网关 key、额度 Plan、用量统计。全部落在 Redis。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	keyPrefix   = "gw:"
	statsTTL    = 400 * 24 * time.Hour
	keyIdxField = "gw:key:idx"
	keySetName  = "gw:keys"
	planSetName = "gw:plans"
)

var (
	ErrKeyNotFound  = errors.New("网关 key 不存在")
	ErrPlanNotFound = errors.New("额度方案不存在")
	ErrKeyDisabled  = errors.New("网关 key 已停用")
)

// ---------------------------------------------------------------------------
// 模型
// ---------------------------------------------------------------------------

// Plan 是一套 credits 额度方案（1 credit = 0.000001 USD）。
// Month / Week / FiveHour 都是 credits；模型按价格自动换算消耗倍率。
type Plan struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Month         int64  `json:"month"`
	Week          int64  `json:"week"`
	FiveHour      int64  `json:"five_hour"`
	Builtin       bool   `json:"builtin,omitempty"`
	SchemaVersion int    `json:"schema_version,omitempty"`
}

// APIKey 是网关侧的调用凭据。明文只在创建时返回一次，库里只存 sha256。
type APIKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	Hash      string    `json:"hash,omitempty"`
	PlanID    string    `json:"plan_id"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used,omitempty"`
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

type Store struct {
	rdb *redis.Client
}

func NewStore(rdb *redis.Client) *Store { return &Store{rdb: rdb} }

func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return prefix + hex.EncodeToString(b[:])
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func maskSecret(secret string) string {
	if len(secret) <= 11 {
		return secret
	}
	return secret[:7] + "…" + secret[len(secret)-4:]
}

// ---------------------------------------------------------------------------
// 网关 key
// ---------------------------------------------------------------------------

func (s *Store) CreateKey(ctx context.Context, name, planID string) (*APIKey, string, error) {
	if strings.TrimSpace(name) == "" {
		return nil, "", errors.New("key 名称不能为空")
	}
	if planID == "" {
		planID = "default"
	}
	if _, err := s.GetPlan(ctx, planID); err != nil {
		return nil, "", err
	}

	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return nil, "", err
	}
	secret := "gw-" + hex.EncodeToString(buf)

	k := &APIKey{
		ID:        newID("k_"),
		Name:      strings.TrimSpace(name),
		Prefix:    maskSecret(secret),
		Hash:      hashSecret(secret),
		PlanID:    planID,
		Enabled:   true,
		CreatedAt: time.Now(),
	}
	body, _ := json.Marshal(k)
	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, keyPrefix+"key:"+k.ID, body, 0)
	pipe.HSet(ctx, keyIdxField, k.Hash, k.ID)
	pipe.SAdd(ctx, keySetName, k.ID)
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, "", err
	}
	return k, secret, nil
}

func (s *Store) GetKey(ctx context.Context, id string) (*APIKey, error) {
	raw, err := s.rdb.Get(ctx, keyPrefix+"key:"+id).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	var k APIKey
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, err
	}
	return &k, nil
}

// GetKeyBySecret 用调用方带来的明文 key 反查；只做一次 HGET + 一次 GET。
func (s *Store) GetKeyBySecret(ctx context.Context, secret string) (*APIKey, error) {
	if secret == "" {
		return nil, ErrKeyNotFound
	}
	id, err := s.rdb.HGet(ctx, keyIdxField, hashSecret(secret)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	k, err := s.GetKey(ctx, id)
	if err != nil {
		return nil, err
	}
	if !k.Enabled {
		return nil, ErrKeyDisabled
	}
	return k, nil
}

func (s *Store) ListKeys(ctx context.Context) ([]APIKey, error) {
	ids, err := s.rdb.SMembers(ctx, keySetName).Result()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []APIKey{}, nil
	}
	pipe := s.rdb.Pipeline()
	cmds := make([]*redis.StringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.Get(ctx, keyPrefix+"key:"+id)
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	out := make([]APIKey, 0, len(ids))
	for _, c := range cmds {
		raw, err := c.Bytes()
		if err != nil {
			continue
		}
		var k APIKey
		if json.Unmarshal(raw, &k) == nil {
			k.Hash = ""
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// KeyPatch 描述一次部分更新，nil 表示不改。
type KeyPatch struct {
	Name    *string `json:"name"`
	PlanID  *string `json:"plan_id"`
	Enabled *bool   `json:"enabled"`
}

func (s *Store) UpdateKey(ctx context.Context, id string, p KeyPatch) (*APIKey, error) {
	k, err := s.GetKey(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Name != nil {
		if strings.TrimSpace(*p.Name) == "" {
			return nil, errors.New("key 名称不能为空")
		}
		k.Name = strings.TrimSpace(*p.Name)
	}
	if p.PlanID != nil {
		if _, err := s.GetPlan(ctx, *p.PlanID); err != nil {
			return nil, err
		}
		k.PlanID = *p.PlanID
	}
	if p.Enabled != nil {
		k.Enabled = *p.Enabled
	}
	body, _ := json.Marshal(k)
	if err := s.rdb.Set(ctx, keyPrefix+"key:"+k.ID, body, 0).Err(); err != nil {
		return nil, err
	}
	k.Hash = ""
	return k, nil
}

func (s *Store) DeleteKey(ctx context.Context, id string) error {
	k, err := s.GetKey(ctx, id)
	if err != nil {
		return err
	}
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, keyPrefix+"key:"+id)
	if k.Hash != "" {
		pipe.HDel(ctx, keyIdxField, k.Hash)
	}
	pipe.SRem(ctx, keySetName, id)
	_, err = pipe.Exec(ctx)
	return err
}

// TouchKey 记录最近使用时间（best-effort，失败不影响请求）。
func (s *Store) TouchKey(ctx context.Context, k *APIKey) {
	k.LastUsed = time.Now()
	body, err := json.Marshal(k)
	if err != nil {
		return
	}
	_ = s.rdb.Set(ctx, keyPrefix+"key:"+k.ID, body, 0).Err()
}

// ---------------------------------------------------------------------------
// 额度方案
// ---------------------------------------------------------------------------

func (s *Store) CreatePlan(ctx context.Context, p Plan) (*Plan, error) {
	if strings.TrimSpace(p.Name) == "" {
		return nil, errors.New("方案名称不能为空")
	}
	if p.Month <= 0 || p.Week <= 0 || p.FiveHour <= 0 {
		return nil, errors.New("方案的三档 credits 额度必须为正整数")
	}
	if p.ID == "" {
		p.ID = newID("p_")
	}
	body, _ := json.Marshal(p)
	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, keyPrefix+"plan:"+p.ID, body, 0)
	pipe.SAdd(ctx, planSetName, p.ID)
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) GetPlan(ctx context.Context, id string) (*Plan, error) {
	raw, err := s.rdb.Get(ctx, keyPrefix+"plan:"+id).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrPlanNotFound
	}
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) ListPlans(ctx context.Context) ([]Plan, error) {
	ids, err := s.rdb.SMembers(ctx, planSetName).Result()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []Plan{}, nil
	}
	pipe := s.rdb.Pipeline()
	cmds := make([]*redis.StringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.Get(ctx, keyPrefix+"plan:"+id)
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	out := make([]Plan, 0, len(ids))
	for _, c := range cmds {
		raw, err := c.Bytes()
		if err != nil {
			continue
		}
		var p Plan
		if json.Unmarshal(raw, &p) == nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := planSortRank(out[i]), planSortRank(out[j])
		if ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (s *Store) UpdatePlan(ctx context.Context, id string, p Plan) (*Plan, error) {
	old, err := s.GetPlan(ctx, id)
	if err != nil {
		return nil, err
	}
	p.ID = id
	p.Builtin = old.Builtin
	// 管理台更新请求不带 schema_version；保留旧版本号，避免启动时把用户修改重新迁移掉。
	if p.SchemaVersion == 0 || p.SchemaVersion < old.SchemaVersion {
		p.SchemaVersion = old.SchemaVersion
	}
	if strings.TrimSpace(p.Name) == "" {
		p.Name = old.Name
	}
	if p.Month <= 0 || p.Week <= 0 || p.FiveHour <= 0 {
		return nil, errors.New("方案的三档 credits 额度必须为正整数")
	}
	body, _ := json.Marshal(p)
	if err := s.rdb.Set(ctx, keyPrefix+"plan:"+id, body, 0).Err(); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) DeletePlan(ctx context.Context, id string) error {
	p, err := s.GetPlan(ctx, id)
	if err != nil {
		return err
	}
	if p.Builtin {
		return errors.New("内置方案不能删除")
	}
	keys, err := s.ListKeys(ctx)
	if err != nil {
		return err
	}
	inUse := 0
	for _, k := range keys {
		if k.PlanID == id {
			inUse++
		}
	}
	if inUse > 0 {
		return fmt.Errorf("还有 %d 个 key 正在使用该方案，请先切换", inUse)
	}
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, keyPrefix+"plan:"+id)
	pipe.SRem(ctx, planSetName, id)
	_, err = pipe.Exec(ctx)
	return err
}

// ---------------------------------------------------------------------------
// 引导：首次启动把 config 里的 api_key / quota 落成默认 key 与方案，
// 幂等补齐内置 Plan 预设，并把旧版（token/金额）内置方案一次性迁移到 credits。
// 迁移完成后，管理台的修改不会再被覆盖。
// ---------------------------------------------------------------------------

func (s *Store) Bootstrap(ctx context.Context, cfg *Config) (createdKey bool, changed int, err error) {
	plans, err := s.ListPlans(ctx)
	if err != nil {
		return false, 0, err
	}
	existing := make(map[string]Plan, len(plans)+1)
	for _, p := range plans {
		existing[p.ID] = p
	}

	if len(plans) == 0 {
		p := Plan{
			ID:            "default",
			Name:          "默认方案",
			Month:         cfg.Quota.Month,
			Week:          cfg.Quota.Week,
			FiveHour:      cfg.Quota.FiveHourWindow,
			Builtin:       true,
			SchemaVersion: planSchemaVersion,
		}
		if _, err := s.CreatePlan(ctx, p); err != nil {
			return false, 0, err
		}
		existing["default"] = p
	} else if old, ok := existing["default"]; ok && old.Builtin && old.SchemaVersion < planSchemaVersion {
		// 旧默认方案：按当前 config 的 credits 额度重置一次。
		migrated := Plan{
			ID:            "default",
			Name:          old.Name,
			Month:         cfg.Quota.Month,
			Week:          cfg.Quota.Week,
			FiveHour:      cfg.Quota.FiveHourWindow,
			Builtin:       true,
			SchemaVersion: planSchemaVersion,
		}
		if _, err := s.UpdatePlan(ctx, "default", migrated); err != nil {
			return false, changed, err
		}
		existing["default"] = migrated
		changed++
	}

	for _, p := range presetPlans() {
		if old, ok := existing[p.ID]; ok {
			if old.Builtin && old.SchemaVersion < planSchemaVersion {
				if _, err := s.UpdatePlan(ctx, p.ID, p); err != nil {
					return false, changed, err
				}
				changed++
			}
			continue
		}
		if _, err := s.CreatePlan(ctx, p); err != nil {
			return false, changed, err
		}
		changed++
	}

	keys, err := s.ListKeys(ctx)
	if err != nil {
		return false, changed, err
	}
	if len(keys) == 0 && strings.TrimSpace(cfg.Gateway.APIKey) != "" {
		secret := cfg.Gateway.APIKey
		k := &APIKey{
			ID:        newID("k_"),
			Name:      "默认密钥",
			Prefix:    maskSecret(secret),
			Hash:      hashSecret(secret),
			PlanID:    "default",
			Enabled:   true,
			CreatedAt: time.Now(),
		}
		body, _ := json.Marshal(k)
		pipe := s.rdb.TxPipeline()
		pipe.Set(ctx, keyPrefix+"key:"+k.ID, body, 0)
		pipe.HSet(ctx, keyIdxField, k.Hash, k.ID)
		pipe.SAdd(ctx, keySetName, k.ID)
		if _, err := pipe.Exec(ctx); err != nil {
			return false, changed, err
		}
		return true, changed, nil
	}
	return false, changed, nil
}

// ---------------------------------------------------------------------------
// 用量统计（按天 / key / 模型）
// ---------------------------------------------------------------------------

func statsKey(day string, keyID string) string {
	return "stats:day:" + day + ":" + keyID
}

// RecordUsage 累加一次成功请求的用量。失败只记日志，不影响主流程。
func (s *Store) RecordUsage(ctx context.Context, keyID, model string, tokensIn, tokensOut, costNano int64) {
	day := time.Now().Format("2006-01-02")
	k := statsKey(day, keyID)
	pipe := s.rdb.Pipeline()
	pipe.HIncrBy(ctx, k, model+"|in", tokensIn)
	pipe.HIncrBy(ctx, k, model+"|out", tokensOut)
	pipe.HIncrBy(ctx, k, model+"|cost", costNano)
	pipe.HIncrBy(ctx, k, model+"|req", 1)
	pipe.Expire(ctx, k, statsTTL)
	_, _ = pipe.Exec(ctx)
}

type UsagePoint struct {
	Date      string  `json:"date,omitempty"`
	Requests  int64   `json:"requests"`
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	Tokens    int64   `json:"tokens"`
	Cost      float64 `json:"cost"`
}

type ModelUsage struct {
	Model     string  `json:"model"`
	Requests  int64   `json:"requests"`
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	Tokens    int64   `json:"tokens"`
	Cost      float64 `json:"cost"`
}

type KeyUsage struct {
	KeyID    string  `json:"key_id"`
	Name     string  `json:"name"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
}

type UsageReport struct {
	From   string       `json:"from"`
	To     string       `json:"to"`
	Totals UsagePoint   `json:"totals"`
	Daily  []UsagePoint `json:"daily"`
	Models []ModelUsage `json:"models"`
	Keys   []KeyUsage   `json:"keys"`
}

// QueryUsage 汇总 [from, to] 区间内（含端点）的用量。
func (s *Store) QueryUsage(ctx context.Context, from, to time.Time, keys []APIKey, keyFilter, modelFilter string) (*UsageReport, error) {
	names := make(map[string]string, len(keys))
	for _, k := range keys {
		if keyFilter != "" && k.ID != keyFilter {
			continue
		}
		names[k.ID] = k.Name
	}

	days := make([]string, 0, 32)
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		days = append(days, d.Format("2006-01-02"))
	}

	type cell struct{ day, keyID string }
	cells := make([]cell, 0, len(days)*len(names))
	for _, day := range days {
		for id := range names {
			cells = append(cells, cell{day, id})
		}
	}

	pipe := s.rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(cells))
	for i, c := range cells {
		cmds[i] = pipe.HGetAll(ctx, statsKey(c.day, c.keyID))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}

	daily := make(map[string]*UsagePoint, len(days))
	for _, d := range days {
		daily[d] = &UsagePoint{Date: d}
	}
	models := map[string]*ModelUsage{}
	byKey := map[string]*KeyUsage{}
	for id := range names {
		byKey[id] = &KeyUsage{KeyID: id, Name: names[id]}
	}

	for i, c := range cells {
		vals := cmds[i].Val()
		if len(vals) == 0 {
			continue
		}
		for field, raw := range vals {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				continue
			}
			idx := strings.LastIndexByte(field, '|')
			if idx <= 0 {
				continue
			}
			model, metric := field[:idx], field[idx+1:]
			if modelFilter != "" && model != modelFilter {
				continue
			}
			d := daily[c.day]
			mu := models[model]
			if mu == nil {
				mu = &ModelUsage{Model: model}
				models[model] = mu
			}
			ku := byKey[c.keyID]
			if d == nil || ku == nil {
				continue // 理论上不会发生；防御性跳过，避免整页统计因单条脏数据挂掉
			}

			switch metric {
			case "in":
				d.TokensIn += n
				mu.TokensIn += n
				ku.Tokens += n
			case "out":
				d.TokensOut += n
				mu.TokensOut += n
				ku.Tokens += n
			case "cost":
				d.Cost += nanoToMoney(n)
				mu.Cost += nanoToMoney(n)
				ku.Cost += nanoToMoney(n)
			case "req":
				d.Requests += n
				mu.Requests += n
				ku.Requests += n
			}
		}
	}

	report := &UsageReport{
		From:   from.Format("2006-01-02"),
		To:     to.Format("2006-01-02"),
		Daily:  make([]UsagePoint, 0, len(days)),
		Models: make([]ModelUsage, 0, len(models)),
		Keys:   make([]KeyUsage, 0, len(byKey)),
	}
	for _, d := range days {
		p := daily[d]
		p.Tokens = p.TokensIn + p.TokensOut
		p.Cost = round8(p.Cost)
		report.Daily = append(report.Daily, *p)
		report.Totals.Requests += p.Requests
		report.Totals.TokensIn += p.TokensIn
		report.Totals.TokensOut += p.TokensOut
		report.Totals.Tokens += p.Tokens
		report.Totals.Cost += p.Cost
	}
	for _, mu := range models {
		mu.Tokens = mu.TokensIn + mu.TokensOut
		mu.Cost = round8(mu.Cost)
		report.Models = append(report.Models, *mu)
	}
	sort.Slice(report.Models, func(i, j int) bool { return report.Models[i].Cost > report.Models[j].Cost })

	for _, ku := range byKey {
		ku.Cost = round8(ku.Cost)
		report.Keys = append(report.Keys, *ku)
	}
	sort.Slice(report.Keys, func(i, j int) bool { return report.Keys[i].Cost > report.Keys[j].Cost })
	report.Totals.Cost = round8(report.Totals.Cost)
	return report, nil
}
