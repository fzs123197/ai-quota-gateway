package main

// Plan 预设：开箱即用的分档 credits 方案。
//
// 1 credit = 0.000001 USD。所有限额只保留 credits 一套单位；
// 模型按价格自动换算消耗倍率（见 pricing.go 的 ModelRate）。
// 预设以固定 ID 写入 Redis，Builtin=true，不允许删除；管理台可修改额度。

// planSchemaVersion 用于一次性迁移旧的 token/金额方案到 credits 方案。
const planSchemaVersion = 2

type PlanPreset struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Month    int64  `json:"month"`
	Week     int64  `json:"week"`
	FiveHour int64  `json:"five_hour"`
}

type planPresetDef struct {
	ID       string
	Name     string
	Month    int64
	Week     int64
	FiveHour int64
}

// 月 / 周 / 5h 比例固定为 1 : 0.30 : 0.03。
// Max 10x / Max 20x 按 Pro（10M credits ≈ $10/月）的 10x / 20x 设计。
var planPresetDefs = []planPresetDef{
	{ID: "go", Name: "Go", Month: 2_000_000, Week: 600_000, FiveHour: 60_000},
	{ID: "plus", Name: "Plus", Month: 5_000_000, Week: 1_500_000, FiveHour: 150_000},
	{ID: "pro", Name: "Pro", Month: 10_000_000, Week: 3_000_000, FiveHour: 300_000},
	{ID: "max10x", Name: "Max 10x", Month: 100_000_000, Week: 30_000_000, FiveHour: 3_000_000},
	{ID: "max20x", Name: "Max 20x", Month: 200_000_000, Week: 60_000_000, FiveHour: 6_000_000},
}

func presetRank(id string) int {
	for i, d := range planPresetDefs {
		if d.ID == id {
			return i
		}
	}
	return -1
}

// planSortRank 让管理台按 Go → Plus → Pro → Max 10x → Max 20x → 自定义 → 默认方案 排序。
func planSortRank(p Plan) int {
	if r := presetRank(p.ID); r >= 0 {
		return r
	}
	if p.ID == "default" {
		return 1000
	}
	if !p.Builtin {
		return 100
	}
	return 500
}

// defaultPlanPresets 返回代码内置的预设值（用于首次落库和接口兜底）。
func defaultPlanPresets() []PlanPreset {
	out := make([]PlanPreset, 0, len(planPresetDefs))
	for _, d := range planPresetDefs {
		out = append(out, PlanPreset{
			ID:       d.ID,
			Name:     d.Name,
			Month:    d.Month,
			Week:     d.Week,
			FiveHour: d.FiveHour,
		})
	}
	return out
}

// presetPlans 把预设转成待写入 Redis 的 Plan。
func presetPlans() []Plan {
	presets := defaultPlanPresets()
	out := make([]Plan, 0, len(presets))
	for _, p := range presets {
		out = append(out, Plan{
			ID: p.ID, Name: p.Name,
			Month: p.Month, Week: p.Week, FiveHour: p.FiveHour,
			Builtin: true, SchemaVersion: planSchemaVersion,
		})
	}
	return out
}

// effectivePlanPresets 优先使用 Redis 中已存在的同名方案值，
// 这样管理台修改预设后，预设选择器展示的就是最新值。
func effectivePlanPresets(plans []Plan) []PlanPreset {
	byID := make(map[string]Plan, len(plans))
	for _, p := range plans {
		byID[p.ID] = p
	}
	defaults := defaultPlanPresets()
	out := make([]PlanPreset, 0, len(planPresetDefs))
	for i, d := range planPresetDefs {
		if p, ok := byID[d.ID]; ok {
			out = append(out, PlanPreset{
				ID: d.ID, Name: p.Name,
				Month: p.Month, Week: p.Week, FiveHour: p.FiveHour,
			})
			continue
		}
		out = append(out, defaults[i])
	}
	return out
}
