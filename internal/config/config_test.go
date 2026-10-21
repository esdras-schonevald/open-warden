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
