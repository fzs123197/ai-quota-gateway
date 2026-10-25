package main

import (
	"io"
	"log/slog"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCreditsFromUsage(t *testing.T) {
	pb := &PriceBook{
		cfg: PricingConfig{Enabled: true, Currency: "USD"},
		quotes: map[string]Quote{
			"m": {Price: Price{Input: 0.27, Output: 1.10, CacheRead: 0.027}},
		},
	}
	q := pb.quotes["m"]

	// 1M 输入 + 1M 输出 = 0.27 + 1.10 = $1.37 = 1,370,000 credits
	got := pb.Credits(q, &Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000})
	if got != 1_370_000 {
		t.Fatalf("credits = %d, want 1370000", got)
	}

	// 50% 输入命中缓存：0.5M*0.27 + 0.5M*0.027 + 1M*1.10 = 1.2485 -> 1,248,500
	got = pb.Credits(q, &Usage{
		PromptTokens:         1_000_000,
		CompletionTokens:     1_000_000,
		PromptCacheHitTokens: 500_000,
	})
	if got != 1_248_500 {
		t.Fatalf("cached credits = %d, want 1248500", got)
	}

	// 预估：100k 输入 + 2k 输出 = 0.027 + 0.0022 = 0.0292 -> 29,200
	got = pb.EstimateCredits(q, tokenEstimate{Prompt: 100_000, Completion: 2_000})
	if got != 29_200 {
		t.Fatalf("estimate credits = %d, want 29200", got)
	}
}

func TestUsdToCreditsBounds(t *testing.T) {
	if got := usdToCredits(0); got != 0 {
		t.Fatalf("0 usd = %d", got)
	}
	if got := usdToCredits(-1); got != 0 {
		t.Fatalf("negative usd = %d", got)
	}
	if got := usdToCredits(1e-12); got != 1 {
		t.Fatalf("tiny usd = %d, want 1", got)
	}
}

func TestCreditUnitDisplay(t *testing.T) {
	usd := NewPriceBook(PricingConfig{Enabled: true, Currency: "USD"}, testLogger())
	if got := usd.CreditUnit(); got != 1e-6 {
		t.Fatalf("usd credit unit = %v", got)
	}

	cny := NewPriceBook(PricingConfig{Enabled: true, Currency: "CNY"}, testLogger())
	cny.fx.set(7.2, "test", nil)
	if got := cny.CreditUnit(); got != 7.2e-6 {
		t.Fatalf("cny credit unit = %v", got)
	}
}

func TestPricingInfoRates(t *testing.T) {
	pb := &PriceBook{
		cfg: PricingConfig{Enabled: true, Currency: "USD"},
		quotes: map[string]Quote{
			"b-model": {Price: Price{Input: 2, Output: 8, CacheRead: 0.2}, Provider: "p", Source: "s"},
			"a-model": {Price: Price{Input: 0.27, Output: 1.1, CacheRead: 0.027}},
		},
	}
	info := pb.Info()
	if info.CreditUnitUSD != 1e-6 || info.CreditUnit != 1e-6 {
		t.Fatalf("credit unit = %v / %v", info.CreditUnitUSD, info.CreditUnit)
	}
	if len(info.Rates) != 2 {
		t.Fatalf("rates = %+v", info.Rates)
	}
	if info.Rates[0].Model != "a-model" || info.Rates[1].Model != "b-model" {
		t.Fatalf("rates not sorted: %+v", info.Rates)
	}
	a, b := info.Rates[0], info.Rates[1]
	// 倍率数值 = 美元 / 1M tokens = credits / token
	if a.Input != 0.27 || a.Output != 1.1 || a.CacheRead != 0.027 {
		t.Fatalf("a rate = %+v", a)
	}
	if b.Input != 2 || b.Output != 8 || b.Provider != "p" || b.Source != "s" {
		t.Fatalf("b rate = %+v", b)
	}
}
