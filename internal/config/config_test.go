package config

import "testing"

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

func TestLimitsFor(t *testing.T) {
	l := &Limits{RPM: 10, TPM: 100, Models: map[string]*Limits{
		"m": {RPM: 2},
	}}
	got := l.For("m")
	if got.RPM != 2 || got.TPM != 100 {
		t.Fatalf("override merge wrong: %+v", got)
	}
	got = l.For("m#high")
	if got.RPM != 2 || got.TPM != 100 {
		t.Fatalf("variant should use base model limits: %+v", got)
	}
}

func TestHasModel(t *testing.T) {
	c := &Config{}
	if c.HasModel() {
		t.Fatal("empty config should not report a model")
	}
	c.Model = "x"
	if !c.HasModel() {
		t.Fatal("model should be detected")
	}
}

func TestEconomyDefaults(t *testing.T) {
	balanced := &Economy{}
	balanced.Defaults()
	if balanced.Mode != "balanced" || !balanced.On() {
		t.Fatalf("balanced mode not applied: %+v", balanced)
	}
	if balanced.ToolOutput.MaxLines != 200 || balanced.Compaction.KeepTokens != 8000 ||
		balanced.MaxOutputTokens == nil || *balanced.MaxOutputTokens != 32768 {
		t.Fatalf("balanced defaults wrong: %+v", balanced)
	}
	agg := &Economy{Mode: "aggressive"}
	agg.Defaults()
	if agg.ToolOutput.MaxLines != 100 || agg.Compaction.KeepTokens != 4000 ||
		*agg.MaxOutputTokens != 16384 {
		t.Fatalf("aggressive defaults wrong: %+v", agg)
	}
	off := &Economy{Mode: "off"}
	off.Defaults()
	if off.On() || off.OutputCap() != 0 || off.ToolOutput != nil {
		t.Fatalf("off should not apply defaults: %+v", off)
	}
	custom := 0
	zero := &Economy{Mode: "balanced", MaxOutputTokens: &custom}
	zero.Defaults()
	if zero.OutputCap() != 0 {
		t.Fatal("explicit zero cap should be preserved")
	}
}

func TestEconomyValidate(t *testing.T) {
	c := &Config{Model: "x", Economy: Economy{Mode: "invalid"}}
	if err := c.Validate(); err == nil {
		t.Fatal("invalid economy mode must fail validation")
	}
	c.Economy.Mode = "off"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
