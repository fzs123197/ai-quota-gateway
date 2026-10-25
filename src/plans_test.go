package main

import "testing"

func TestPlanPresetsDistinctAndOrdered(t *testing.T) {
	presets := defaultPlanPresets()
	wantIDs := []string{"go", "plus", "pro", "max10x", "max20x"}
	wantNames := []string{"Go", "Plus", "Pro", "Max 10x", "Max 20x"}
	if len(presets) != len(wantIDs) {
		t.Fatalf("presets = %d", len(presets))
	}
	months := map[int64]bool{}
	weeks := map[int64]bool{}
	fives := map[int64]bool{}
	for i, p := range presets {
		if p.ID != wantIDs[i] || p.Name != wantNames[i] {
			t.Fatalf("preset[%d] = %s/%s", i, p.ID, p.Name)
		}
		if p.Month <= 0 || p.Week <= 0 || p.FiveHour <= 0 {
			t.Fatalf("preset %s has non-positive quota", p.ID)
		}
		if months[p.Month] || weeks[p.Week] || fives[p.FiveHour] {
			t.Fatalf("preset %s duplicates another tier", p.ID)
		}
		months[p.Month], weeks[p.Week], fives[p.FiveHour] = true, true, true
		if i > 0 {
			prev := presets[i-1]
			if p.Month <= prev.Month || p.Week <= prev.Week || p.FiveHour <= prev.FiveHour {
				t.Fatalf("preset %s must be larger than %s", p.ID, prev.ID)
			}
		}
	}
}

func TestPlanPresetRatios(t *testing.T) {
	for _, p := range defaultPlanPresets() {
		// 月 : 周 : 5h = 100 : 30 : 3
		if p.Week*100 != p.Month*30 {
			t.Fatalf("%s week ratio: %d vs month %d", p.ID, p.Week, p.Month)
		}
		if p.FiveHour*100 != p.Month*3 {
			t.Fatalf("%s five-hour ratio: %d vs month %d", p.ID, p.FiveHour, p.Month)
		}
	}
}

func TestPlanTierMultipliers(t *testing.T) {
	byID := map[string]PlanPreset{}
	for _, p := range defaultPlanPresets() {
		byID[p.ID] = p
	}
	pro := byID["pro"].Month
	if got := byID["max10x"].Month; got != pro*10 {
		t.Fatalf("max10x = %d, want 10x pro (%d)", got, pro*10)
	}
	if got := byID["max20x"].Month; got != pro*20 {
		t.Fatalf("max20x = %d, want 20x pro (%d)", got, pro*20)
	}
	if byID["plus"].Month <= byID["go"].Month || byID["pro"].Month <= byID["plus"].Month {
		t.Fatalf("tiers are not increasing: %+v", byID)
	}
}

func TestPresetPlansAreBuiltin(t *testing.T) {
	for _, p := range presetPlans() {
		if !p.Builtin {
			t.Fatalf("preset %s is not builtin", p.ID)
		}
		if p.SchemaVersion != planSchemaVersion {
			t.Fatalf("preset %s schema version = %d", p.ID, p.SchemaVersion)
		}
		if p.ID == "" || p.Name == "" || p.Month <= 0 {
			t.Fatalf("invalid preset %+v", p)
		}
	}
}

func TestPlanSortRank(t *testing.T) {
	want := []struct {
		id   string
		plan Plan
		rank int
	}{
		{"go", Plan{ID: "go"}, 0},
		{"plus", Plan{ID: "plus"}, 1},
		{"pro", Plan{ID: "pro"}, 2},
		{"max10x", Plan{ID: "max10x"}, 3},
		{"max20x", Plan{ID: "max20x"}, 4},
		{"custom", Plan{ID: "custom"}, 100},
		{"other-builtin", Plan{ID: "other", Builtin: true}, 500},
		{"default", Plan{ID: "default", Builtin: true}, 1000},
	}
	for _, w := range want {
		if got := planSortRank(w.plan); got != w.rank {
			t.Fatalf("%s rank = %d, want %d", w.id, got, w.rank)
		}
	}
}

func TestEffectivePresetsUsePersistedValues(t *testing.T) {
	edited := Plan{ID: "go", Name: "Go 自定义", Month: 111, Week: 22, FiveHour: 3}
	got := effectivePlanPresets([]Plan{edited})
	if len(got) != len(planPresetDefs) {
		t.Fatalf("presets = %d", len(got))
	}
	if p := got[0]; p.ID != "go" || p.Name != "Go 自定义" || p.Month != 111 || p.Week != 22 || p.FiveHour != 3 {
		t.Fatalf("go preset = %+v", p)
	}
	if p := got[1]; p.ID != "plus" || p.Month != 5_000_000 {
		t.Fatalf("plus preset = %+v", p)
	}
}
