package config

import (
	"testing"
)

func TestDefaults(t *testing.T) {
	c := &Config{Model: "m"}
	c.Defaults()
	if c.Run.Timeout <= 0 || c.Run.PollInterval <= 0 || c.Run.OutputHeadroom <= 0 {
		t.Fatalf("defaults not applied: %+v", c.Run)
	}
	if !c.Run.Auto() {
		t.Fatal("auto-approve should default to true")
	}
	if c.StateFile == "" {
		t.Fatal("state file default missing")
	}
}

func TestModelPriceVariant(t *testing.T) {
	c := &Config{Pricing: map[string]Pricing{
		"default":        {Input: 1},
		"prov/model":     {Input: 2},
		"prov/model#max": {Input: 3},
	}}
	if got := c.ModelPrice("prov/model#high").Input; got != 2 {
		t.Fatalf("variant should fall back to base price, got %v", got)
	}
	if got := c.ModelPrice("prov/model#max").Input; got != 3 {
		t.Fatalf("exact variant price should win, got %v", got)
	}
	if got := c.ModelPrice("unknown").Input; got != 1 {
		t.Fatalf("unknown model should use default, got %v", got)
	}
}

func TestLimitsFor(t *testing.T) {
	l := &Limits{RPM: 10, TPM: 100, Models: map[string]*Limits{
		"m": {RPM: 2},
	}}
	got := l.For("m")
	if got.RPM != 2 || got.TPM != 100 {
		t.Fatalf("override merge wrong: %+v", got)
	}
}

func TestGatewayAndCatalog(t *testing.T) {
	c := &Config{
		Model:     "siliconflow/real-model",
		Providers: map[string]Provider{"siliconflow": {Upstream: "https://api.example/v1"}},
		Catalog: []CatalogEntry{
			{Alias: "qwen", Label: "Qwen", Model: "siliconflow/real-model", Context: 1000,
				Pricing: &Pricing{Input: 0.07, Output: 0.28}},
		},
	}
	c.Defaults()
	if !c.GatewayEnabled() {
		t.Fatal("gateway should default to enabled when providers exist")
	}
	if got := c.ResolveModel("qwen"); got != "siliconflow/real-model" {
		t.Fatalf("alias not resolved: %s", got)
	}
	if got := c.ResolveModel("other/model"); got != "other/model" {
		t.Fatalf("raw model changed: %s", got)
	}
	if p := c.EffectivePrice("siliconflow/real-model"); p.Input != 0.07 {
		t.Fatalf("catalog price not used: %+v", p)
	}
	if ProviderOf("siliconflow/real-model") != "siliconflow" {
		t.Fatal("provider extraction failed")
	}
	off := false
	c.Gateway.Enabled = &off
	if c.GatewayEnabled() {
		t.Fatal("explicit disable ignored")
	}
}
