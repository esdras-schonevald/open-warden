package warden

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esdras-schonevald/open-warden/internal/config"
	"github.com/esdras-schonevald/open-warden/internal/usage"
)

// fakeEngine writes a shell script that behaves like the embedded engine:
// it streams one JSON text event and logs the model it was called with.
func fakeEngine(t *testing.T, sleep time.Duration) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "models.log")
	bin = filepath.Join(dir, "fake-engine")
	script := fmt.Sprintf(`#!/bin/bash
sid=""; model=""
while [ $# -gt 0 ]; do
  case "$1" in
    --session) sid="$2"; shift 2 ;;
    --model) model="$2"; shift 2 ;;
    *) shift ;;
  esac
done
echo "$model" >> %q
printf '{"type":"text","sessionID":"%%s","part":{"type":"text","text":"hello from fake"}}\n' "$sid"
sleep %g
exit 0
`, logPath, sleep.Seconds())
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

func testConfig(t *testing.T, bin string) *config.Config {
	t.Helper()
	cfg := &config.Config{
		EngineBin: bin,
		Model:     "primary/model",
		Fallbacks: []string{"fallback/model"},
		Budget:    0.5,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Limits:    &config.Limits{RPM: 100},
		Pricing:   map[string]config.Pricing{},
	}
	cfg.Defaults()
	cfg.Run.Timeout = 10 * time.Second
	cfg.Run.PollInterval = 50 * time.Millisecond
	cfg.Run.AutoApprove = boolPtr(true)
	return cfg
}

func boolPtr(v bool) *bool { return &v }

func zeroUsage(string, time.Time) (*usage.Report, error) {
	return &usage.Report{ByModel: map[string]usage.Totals{}}, nil
}

func TestRunStreamsOutputAndReports(t *testing.T) {
	bin, _ := fakeEngine(t, 0)
	cfg := testConfig(t, bin)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Collect = zeroUsage
	var out strings.Builder
	r.Out = &out

	rep, err := r.Run(context.Background(), Options{Prompt: "do it"})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if rep.Status != "ok" {
		t.Fatalf("status %s", rep.Status)
	}
	if !strings.Contains(out.String(), "hello from fake") {
		t.Fatalf("output not streamed: %q", out.String())
	}
	if rep.Output != "hello from fake" {
		t.Fatalf("report output: %q", rep.Output)
	}
}

func TestProactiveFallbackBeforeRateLimit(t *testing.T) {
	bin, logPath := fakeEngine(t, 0)
	cfg := testConfig(t, bin)
	cfg.Limits = &config.Limits{RPM: 1}
	cfg.Fallback.WaitBlocked = false
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Collect = zeroUsage
	r.Out = &strings.Builder{}

	if _, err := r.Run(context.Background(), Options{Prompt: "first"}); err != nil {
		t.Fatal(err)
	}
	rep, err := r.Run(context.Background(), Options{Prompt: "second"})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(rep.Fallbacks) == 0 {
		t.Fatal("expected a proactive fallback event")
	}
	f := rep.Fallbacks[0]
	if f.Reason != "rate_limit" || f.From != "primary/model" || f.To != "fallback/model" {
		t.Fatalf("unexpected fallback: %+v", f)
	}
	logged, _ := os.ReadFile(logPath)
	lines := strings.Fields(string(logged))
	if len(lines) != 2 || lines[0] != "primary/model" || lines[1] != "fallback/model" {
		t.Fatalf("engine call order wrong: %v", lines)
	}
}

func TestBudgetStopsRun(t *testing.T) {
	bin, _ := fakeEngine(t, 1500*time.Millisecond)
	cfg := testConfig(t, bin)
	cfg.Budget = 0.5
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Out = &strings.Builder{}
	r.Collect = func(string, time.Time) (*usage.Report, error) {
		return &usage.Report{ByModel: map[string]usage.Totals{}, Total: usage.Totals{Cost: 0.6}}, nil
	}
	start := time.Now()
	rep, _ := r.Run(context.Background(), Options{Prompt: "expensive"})
	if time.Since(start) > 1200*time.Millisecond {
		t.Fatal("budget did not interrupt the run promptly")
	}
	if !rep.BudgetExceeded || rep.Status != "budget_exceeded" {
		t.Fatalf("budget not enforced: %+v", rep)
	}
}

func TestComposePromptGlobalAndAgent(t *testing.T) {
	dir := t.TempDir()
	global := filepath.Join(dir, "g.md")
	agent := filepath.Join(dir, "a.md")
	os.WriteFile(global, []byte("GLOBAL"), 0o644)
	os.WriteFile(agent, []byte("AGENT"), 0o644)
	cfg := &config.Config{Prompt: config.Prompt{
		GlobalFile: global,
		Agents:     map[string]config.AgentSpec{"b": {PromptFile: agent}},
	}}
	r := &Runner{Cfg: cfg}
	got, err := r.composePrompt("b", "TASK")
	if err != nil {
		t.Fatal(err)
	}
	if got != "GLOBAL\n\nAGENT\n\nTASK" {
		t.Fatalf("compose: %q", got)
	}
}

func TestChainOrderAndFallback(t *testing.T) {
	cfg := &config.Config{
		Model:     "primary",
		Fallbacks: []string{"f1", "f2"},
	}
	r := &Runner{Cfg: cfg}
	chain := r.chain(Options{}, config.AgentSpec{Fallback: "agent-fallback"})
	want := []string{"primary", "agent-fallback", "f1", "f2"}
	if strings.Join(chain, ",") != strings.Join(want, ",") {
		t.Fatalf("chain=%v", chain)
	}
}

func TestSubagentPolicyMovesOnFallback(t *testing.T) {
	cfg := &config.Config{Model: "primary", Subagents: config.Subagents{
		Model: "sub-primary", FallbackModel: "sub-fallback",
	}}
	r := &Runner{Cfg: cfg}
	if got := r.subagentModel("primary"); got != "sub-primary" {
		t.Fatalf("primary subagent model: %s", got)
	}
	if got := r.subagentModel("fallback"); got != "sub-fallback" {
		t.Fatalf("fallback subagent model: %s", got)
	}
}
