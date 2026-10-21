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
