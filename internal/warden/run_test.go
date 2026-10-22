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
	"github.com/esdras-schonevald/open-warden/internal/limiter"
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
		Model:     "qwen-coder-30b",
		Fallbacks: []string{"deepseek-flash"},
		Budget:    0.5,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
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
	cfg.Fallback.WaitBlocked = false
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Lim = limiter.New(func(m string) config.Limits {
		return config.Limits{RPM: 1}
	}, nil)
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
	if f.Reason != "rate_limit" || f.From != "qwen-coder-30b" || f.To != "deepseek-flash" {
		t.Fatalf("unexpected fallback: %+v", f)
	}
	logged, _ := os.ReadFile(logPath)
	lines := strings.Fields(string(logged))
	if len(lines) != 2 ||
		lines[0] != "siliconflow/Qwen/Qwen3-Coder-30B-A3B-Instruct" ||
		lines[1] != "deepseek/deepseek-flash" {
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
	if !strings.Contains(got, "GLOBAL") || !strings.Contains(got, "AGENT") ||
		!strings.Contains(got, "SUBAGENT POLICY") || !strings.Contains(got, "TASK") {
		t.Fatalf("compose missing sections: %q", got)
	}
	if strings.Index(got, "GLOBAL") > strings.Index(got, "AGENT") ||
		strings.Index(got, "AGENT") > strings.Index(got, "SUBAGENT POLICY") ||
		strings.Index(got, "SUBAGENT POLICY") > strings.Index(got, "TASK") {
		t.Fatalf("compose order wrong: %q", got)
	}
}

func TestChainOrderAndFallback(t *testing.T) {
	cfg := &config.Config{
		Model:     "qwen-coder-30b",
		Fallbacks: []string{"deepseek-flash"},
	}
	r := &Runner{Cfg: cfg}
	chain := r.chain(Options{}, config.AgentSpec{Fallback: "glm-5.3-flash"})
	want := []string{
		"siliconflow/Qwen/Qwen3-Coder-30B-A3B-Instruct",
		"siliconflow/zai-org/GLM-5.3-Flash",
		"deepseek/deepseek-flash",
	}
	if strings.Join(chain, ",") != strings.Join(want, ",") {
		t.Fatalf("chain=%v", chain)
	}
}

func TestSubagentFallbackWhenExhausted(t *testing.T) {
	cfg := &config.Config{
		Model: "free-glm",
		Subagents: config.Subagents{
			Model: "free-glm", FallbackModel: "free-deepseek",
		},
	}
	lim := limiter.New(func(m string) config.Limits {
		if m == "opencode/glm-5.3-flash-free" {
			return config.Limits{RPM: 1}
		}
		return config.Limits{}
	}, nil)
	lim.Consume("opencode/glm-5.3-flash-free", 10)
	r := &Runner{Cfg: cfg, Lim: lim}
	got, exhausted := r.chooseSubagentModel("opencode/glm-5.3-flash-free", 100)
	if got != "opencode/deepseek-v4.1-flash-free" || exhausted != "opencode/glm-5.3-flash-free" {
		t.Fatalf("got=%s exhausted=%s", got, exhausted)
	}
}

func TestSubagentPolicyMovesOnFallback(t *testing.T) {
	cfg := &config.Config{Model: "qwen-coder-30b", Subagents: config.Subagents{
		Model: "glm-5.3-flash", FallbackModel: "deepseek-flash",
	}}
	r := &Runner{Cfg: cfg}
	if got := r.subagentModel("siliconflow/Qwen/Qwen3-Coder-30B-A3B-Instruct"); got != "siliconflow/zai-org/GLM-5.3-Flash" {
		t.Fatalf("primary subagent model: %s", got)
	}
	if got := r.subagentModel("deepseek/deepseek-flash"); got != "deepseek/deepseek-flash" {
		t.Fatalf("fallback subagent model: %s", got)
	}
}

func TestProviderOverridesOnlyWhenGatewaySet(t *testing.T) {
	r := &Runner{Cfg: &config.Config{Model: "qwen-coder-30b"}}
	if got := r.providerOverrides(); got != nil {
		t.Fatalf("unexpected overrides without gateway: %v", got)
	}
	r.GatewayURL = "http://127.0.0.1:1234"
	r.GatewayToken = "tok"
	got := r.providerOverrides()
	if got["siliconflow"].BaseURL != "http://127.0.0.1:1234/siliconflow/v1" {
		t.Fatalf("bad base url: %+v", got)
	}
	if got["anthropic"].BaseURL != "http://127.0.0.1:1234/anthropic" {
		t.Fatalf("bad anthropic base url: %+v", got)
	}
	if got["free-tier"].BaseURL != "" {
		t.Fatalf("free-tier must not be routed")
	}
	if got["siliconflow"].Headers["x-warden-gateway"] != "tok" {
		t.Fatalf("missing gateway token header")
	}
	if !r.routed("siliconflow/Qwen/Qwen3-Coder-30B-A3B-Instruct") {
		t.Fatal("paid provider should be routed")
	}
	if r.routed("opencode/glm-5.3-flash-free") {
		t.Fatal("free tier must stay direct")
	}
	if r.routed("unknown/model") {
		t.Fatal("unknown provider must not be routed")
	}
}

func TestChainResolvesAliases(t *testing.T) {
	cfg := &config.Config{
		Model:     "qwen-coder-30b",
		Fallbacks: []string{"deepseek-flash"},
	}
	r := &Runner{Cfg: cfg}
	chain := r.chain(Options{Model: "glm-5.3-flash"}, config.AgentSpec{})
	want := []string{"siliconflow/zai-org/GLM-5.3-Flash", "deepseek/deepseek-flash"}
	if strings.Join(chain, ",") != strings.Join(want, ",") {
		t.Fatalf("alias chain wrong: %v", chain)
	}
}

func TestAPIKeyScopedToSelectedProvider(t *testing.T) {
	r := &Runner{APIKey: "secret", APIKeyProvider: "siliconflow"}
	if env, val := r.apiKeyEnv("siliconflow/Qwen/Qwen3-Coder-30B-A3B-Instruct"); env != "SILICONFLOW_API_KEY" || val != "secret" {
		t.Fatalf("primary key env: %q %q", env, val)
	}
	if env, _ := r.apiKeyEnv("deepseek/deepseek-flash"); env != "" {
		t.Fatalf("fallback provider must not receive the key: %q", env)
	}
	if env, _ := r.apiKeyEnv("opencode/glm-5.3-flash-free"); env != "" {
		t.Fatalf("free tier must not receive the key: %q", env)
	}
}

func TestComposePromptEconomyModes(t *testing.T) {
	cfg := &config.Config{Prompt: config.Prompt{Global: "G"}}
	off := &Runner{Cfg: cfg}
	off.Cfg.Economy.Mode = "off"
	got, err := off.composePrompt("", "T")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "SUBAGENT POLICY") {
		t.Fatal("delegation policy must always be present")
	}
	if strings.Contains(got, "TOKEN ECONOMY") {
		t.Fatal("economy policy must be absent when off")
	}

	balanced := &Runner{Cfg: cfg}
	balanced.Cfg.Economy.Mode = "balanced"
	got, _ = balanced.composePrompt("", "T")
	if !strings.Contains(got, "TOKEN ECONOMY") || !strings.Contains(got, "BALANCED") {
		t.Fatal("balanced economy policy missing")
	}

	aggressive := &Runner{Cfg: cfg}
	aggressive.Cfg.Economy.Mode = "aggressive"
	got, _ = aggressive.composePrompt("", "T")
	if !strings.Contains(got, "AGGRESSIVE") {
		t.Fatal("aggressive economy policy missing")
	}
}
