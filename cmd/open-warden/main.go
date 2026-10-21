// Command open-warden is a CLI that runs guarded, budgeted agents on top of an
// embedded inference engine, with proactive rate limiting and fallback routing.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/esdras-schonevald/open-warden/internal/config"
	"github.com/esdras-schonevald/open-warden/internal/gateway"
	"github.com/esdras-schonevald/open-warden/internal/report"
	"github.com/esdras-schonevald/open-warden/internal/state"
	"github.com/esdras-schonevald/open-warden/internal/usage"
	"github.com/esdras-schonevald/open-warden/internal/warden"
)

var version = "0.2.0"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usageText()
		return 2
	}
	cmd, rest := args[0], args[1:]
	var code int
	switch cmd {
	case "run":
		code = cmdRun(rest, false)
	case "agents":
		code = cmdRun(rest, true)
	case "report":
		code = cmdReport(rest)
	case "validate":
		code = cmdValidate(rest)
	case "models":
		code = cmdModels(rest)
	case "providers":
		code = cmdProviders(rest)
	case "doctor":
		code = cmdDoctor(rest)
	case "version", "--version", "-v":
		fmt.Printf("open-warden %s\n", version)
	case "help", "--help", "-h":
		usageText()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usageText()
		code = 2
	}
	return code
}

func usageText() {
	fmt.Print(`open-warden — guarded agent execution

Usage:
  open-warden run [prompt...]       Run one agent (proactive limits + fallback)
  open-warden agents [task...]      Run every configured agent in parallel
  open-warden models                List available models (alias, context, price)
  open-warden providers             Show provider credentials status
  open-warden doctor                Check engine, credentials and state
  open-warden report                Show token/cost report from run history
  open-warden validate              Validate configuration and show effective policy
  open-warden help [command]        Show help
  open-warden version

Common flags for run/agents:
  -c, --config <file>        configuration file (default: ./warden.yaml)
      --agent <name>         agent profile to use
  -m, --model <model>        override primary model
      --fallback <model>     override first fallback model
  -b, --budget <usd>         per-run cost limit (e.g. 2.00)
  -w, --workdir <dir>        working directory for the agent
      --prompt-file <file>   read the task prompt from a file
      --timeout <duration>   run timeout (e.g. 20m)
      --json                 machine-readable output (includes full report)
  -q, --quiet                do not stream assistant text

Prompt is taken from arguments, --prompt-file, or stdin (when piped).

Exit codes: 0 ok, 1 failure, 2 usage, 3 budget reached.

Examples:
  open-warden models
  open-warden run -c warden.yaml -m qwen-coder-30b "Fix the failing tests"
  open-warden agents -c warden.yaml --budget 2.00 "Implement TASK.md"
  open-warden report -c warden.yaml --last 20
`)
}

type runFlags struct {
	config     string
	agent      string
	model      string
	fallback   string
	budget     float64
	workdir    string
	promptFile string
	jsonOut    bool
	quiet      bool
	timeout    time.Duration
}

func parseRunFlags(fs *flag.FlagSet) *runFlags {
	rf := &runFlags{}
	fs.StringVar(&rf.config, "config", "", "configuration file")
	fs.StringVar(&rf.config, "c", "", "configuration file")
	fs.StringVar(&rf.agent, "agent", "", "agent profile")
	fs.StringVar(&rf.model, "model", "", "primary model override")
	fs.StringVar(&rf.model, "m", "", "primary model override")
	fs.StringVar(&rf.fallback, "fallback", "", "first fallback override")
	fs.Float64Var(&rf.budget, "budget", 0, "per-run budget in USD")
	fs.Float64Var(&rf.budget, "b", 0, "per-run budget in USD")
	fs.StringVar(&rf.workdir, "workdir", "", "agent working directory")
	fs.StringVar(&rf.workdir, "w", "", "agent working directory")
	fs.StringVar(&rf.promptFile, "prompt-file", "", "prompt file")
	fs.BoolVar(&rf.jsonOut, "json", false, "JSON output")
	fs.BoolVar(&rf.quiet, "quiet", false, "do not stream text")
	fs.BoolVar(&rf.quiet, "q", false, "do not stream text")
	fs.DurationVar(&rf.timeout, "timeout", 0, "run timeout")
	return rf
}

func cmdRun(args []string, multi bool) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = usageText
	rf := parseRunFlags(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cfg, err := loadConfig(rf.config, rf.model, rf.budget)
	if err != nil {
		return fail(err)
	}
	applySecretFiles(cfg)
	if rf.fallback != "" {
		cfg.Fallbacks = append([]string{rf.fallback}, cfg.Fallbacks...)
	}
	if rf.timeout > 0 {
		cfg.Run.Timeout = rf.timeout
	}
	prompt, err := readPrompt(fs.Args(), rf.promptFile)
	if err != nil {
		return fail(err)
	}
	if strings.TrimSpace(prompt) == "" && !multi {
		return fail(fmt.Errorf("no prompt provided"))
	}

	runner, err := warden.New(cfg)
	if err != nil {
		return fail(err)
	}
	if cfg.GatewayEnabled() && len(cfg.Providers) > 0 {
		gw, err := gateway.New(cfg, runner.Lim)
		if err != nil {
			return fail(err)
		}
		defer gw.Close()
		runner.GatewayURL = gw.URL
		runner.GatewayToken = gw.Token
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := warden.Options{
		Agent:   rf.agent,
		Prompt:  prompt,
		Model:   rf.model,
		Budget:  rf.budget,
		Workdir: rf.workdir,
		JSON:    rf.jsonOut,
		Quiet:   rf.quiet,
	}

	if !multi {
		rep, err := runner.Run(ctx, opts)
		printRun(rep, rf.jsonOut, rf.quiet)
		return exitFor(rep, err)
	}

	runs, err := runner.RunAgents(ctx, prompt, opts)
	printAgents(runs, rf.jsonOut)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	for _, rep := range runs {
		if rep != nil && rep.BudgetExceeded {
			return 3
		}
	}
	if err != nil {
		return 1
	}
	return 0
}

func printRun(rep *report.Run, jsonOut, quiet bool) {
	if rep == nil {
		return
	}
	if jsonOut {
		out, _ := rep.JSON()
		fmt.Println(out)
		return
	}
	if quiet {
		fmt.Println(rep.Output)
	}
	fmt.Print(rep.Text())
}

func printAgents(runs []*report.Run, jsonOut bool) {
	if jsonOut {
		type agg struct {
			Runs  []*report.Run `json:"runs"`
			Total usage.Totals  `json:"total"`
		}
		a := agg{Runs: runs}
		for _, r := range runs {
			if r != nil && r.Usage != nil {
				a.Total.Add(r.Usage.Total)
			}
		}
		raw, _ := json.MarshalIndent(a, "", "  ")
		fmt.Println(string(raw))
		return
	}
	for _, r := range runs {
		if r == nil {
			continue
		}
		fmt.Printf("\n===== agent: %s =====\n%s", r.Agent, r.Output)
		fmt.Print(r.Text())
	}
	fmt.Printf("\n===== consolidated =====\n")
	printTotals(runs)
}

func printTotals(runs []*report.Run) {
	var total usage.Totals
	for _, r := range runs {
		if r != nil && r.Usage != nil {
			total.Add(r.Usage.Total)
		}
	}
	fmt.Printf("agents=%d requests=%d in=%d out=%d reasoning=%d cache_read=%d cache_write=%d cost=$%.6f\n",
		len(runs), total.Requests, total.Input, total.Output, total.Reasoning,
		total.CacheRead, total.CacheWrite, total.Cost)
}

func exitFor(rep *report.Run, err error) int {
	if rep != nil && rep.BudgetExceeded {
		return 3
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if rep != nil && rep.Status != "ok" && rep.Status != "ok_with_fallback" {
		return 1
	}
	return 0
}

func readPrompt(args []string, file string) (string, error) {
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read prompt file: %w", err)
		}
		return strings.TrimSpace(string(raw)), nil
	}
	if len(args) > 0 {
		return strings.Join(args, " "), nil
	}
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice == 0 {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
	return "", nil
}

// loadConfig finds and loads configuration; CLI overrides win.
func loadConfig(path, model string, budget float64) (*config.Config, error) {
	if path == "" {
		path = os.Getenv("WARDEN_CONFIG")
	}
	if path == "" {
		for _, c := range []string{"warden.yaml", "warden.yml", "open-warden.yaml"} {
			if _, err := os.Stat(c); err == nil {
				path = c
				break
			}
		}
	}
	var cfg *config.Config
	if path != "" {
		var err error
		cfg, err = config.Load(path)
		if err != nil {
			return nil, err
		}
	} else {
		cfg = &config.Config{}
		cfg.Defaults()
	}
	if model != "" {
		cfg.Model = model
	}
	if budget > 0 {
		cfg.Budget = budget
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("no model configured: set model in config or pass -m provider/model")
	}
	return cfg, nil
}

func cmdReport(args []string) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.Usage = usageText
	var cfgPath, statePath string
	var asJSON bool
	var last int
	fs.StringVar(&cfgPath, "config", "", "configuration file")
	fs.StringVar(&cfgPath, "c", "", "configuration file")
	fs.StringVar(&statePath, "state", "", "state file")
	fs.BoolVar(&asJSON, "json", false, "JSON output")
	fs.IntVar(&last, "last", 20, "number of recent runs")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if statePath == "" {
		cfg, err := loadConfig(cfgPath, "unused", 0)
		if err == nil {
			statePath = cfg.StateFile
		} else {
			c := &config.Config{}
			c.Defaults()
			statePath = c.StateFile
		}
	}
	st, err := state.Load(statePath)
	if err != nil {
		return fail(err)
	}
	if asJSON {
		raw, _ := json.MarshalIndent(st, "", "  ")
		fmt.Println(string(raw))
		return 0
	}
	printHistory(st.History, last)
	return 0
}

func printHistory(history []state.RunRecord, last int) {
	if len(history) == 0 {
		fmt.Println("no runs recorded yet")
		return
	}
	var total state.RunRecord
	for _, r := range history {
		total.Input += r.Input
		total.Output += r.Output
		total.Reasoning += r.Reasoning
		total.CacheRead += r.CacheRead
		total.CacheWrite += r.CacheWrite
		total.SubInput += r.SubInput
		total.SubOutput += r.SubOutput
		total.SubReasoning += r.SubReasoning
		total.SubCacheRead += r.SubCacheRead
		total.SubCacheWrite += r.SubCacheWrite
		total.Cost += r.Cost
		total.Requests += r.Requests
	}
	fmt.Printf("runs=%d requests=%d cost=$%.6f\n", len(history), total.Requests, total.Cost)
	fmt.Printf("tokens: in=%d out=%d reasoning=%d cache_read=%d cache_write=%d\n",
		total.Input, total.Output, total.Reasoning, total.CacheRead, total.CacheWrite)
	fmt.Printf("subagents: in=%d out=%d reasoning=%d cache_read=%d cache_write=%d\n\n",
		total.SubInput, total.SubOutput, total.SubReasoning, total.SubCacheRead, total.SubCacheWrite)

	start := 0
	if last > 0 && len(history) > last {
		start = len(history) - last
	}
	fmt.Printf("%-22s %-12s %-18s %9s %9s %8s %10s %10s\n",
		"run", "agent", "status", "in", "out", "reason", "cost", "dur")
	for _, r := range history[start:] {
		fmt.Printf("%-22s %-12s %-18s %9d %9d %8d %10.6f %9.1fs\n",
			short(r.ID), r.Agent, r.Status, r.Input, r.Output, r.Reasoning, r.Cost, r.DurationSec)
	}
}

func short(id string) string {
	if len(id) > 20 {
		return id[:20]
	}
	return id
}

func cmdValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.Usage = usageText
	var cfgPath string
	fs.StringVar(&cfgPath, "config", "", "configuration file")
	fs.StringVar(&cfgPath, "c", "", "configuration file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cfg, err := loadConfig(cfgPath, "", 0)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("config: %s\n", coalesce(cfgPath, "defaults"))
	fmt.Printf("model:  %s\n", cfg.Model)
	fmt.Printf("fallbacks: %s\n", strings.Join(cfg.Fallbacks, " -> "))
	fmt.Printf("budget/run: $%.2f\n", cfg.Budget)
	fmt.Printf("limits: rpm=%d tpm=%d rpd=%d tpd=%d\n",
		cfg.Limits.RPM, cfg.Limits.TPM, cfg.Limits.RPD, cfg.Limits.TPD)
	if len(cfg.Limits.Models) > 0 {
		keys := make([]string, 0, len(cfg.Limits.Models))
		for k := range cfg.Limits.Models {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			l := cfg.Limits.For(k)
			fmt.Printf("  %-45s rpm=%d tpm=%d rpd=%d tpd=%d\n", k, l.RPM, l.TPM, l.RPD, l.TPD)
		}
	}
	fmt.Printf("subagents: model=%s fallback=%s\n", coalesce(cfg.Subagents.Model, "-"), coalesce(cfg.Subagents.FallbackModel, "-"))
	fmt.Printf("gateway: %v (providers=%d)\n", cfg.GatewayEnabled(), len(cfg.Providers))
	fmt.Printf("catalog: %d models exposed\n", len(cfg.Catalog))
	agents := (&warden.Runner{Cfg: cfg}).AgentNames("")
	fmt.Printf("agents: %s\n", strings.Join(agents, ", "))
	return 0
}

func coalesce(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

type publicModel struct {
	Alias   string  `json:"alias"`
	Label   string  `json:"label"`
	Context int     `json:"context"`
	Input   float64 `json:"input_per_million"`
	Output  float64 `json:"output_per_million"`
}

func cmdModels(args []string) int {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = usageText
	var cfgPath string
	var asJSON bool
	fs.StringVar(&cfgPath, "config", "", "configuration file")
	fs.StringVar(&cfgPath, "c", "", "configuration file")
	fs.BoolVar(&asJSON, "json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cfg, err := loadConfig(cfgPath, "", 0)
	if err != nil {
		return fail(err)
	}
	entries := publicModels(cfg)
	if len(fs.Args()) > 0 {
		for _, e := range entries {
			if e.Alias == fs.Args()[0] {
				if asJSON {
					raw, _ := json.MarshalIndent(e, "", "  ")
					fmt.Println(string(raw))
					return 0
				}
				fmt.Printf("alias:   %s\nlabel:   %s\ncontext: %s\ninput:   $%.2f/M\noutput:  $%.2f/M\n",
					e.Alias, e.Label, fmtContext(e.Context), e.Input, e.Output)
				return 0
			}
		}
		return fail(fmt.Errorf("unknown model alias %q", fs.Args()[0]))
	}
	if asJSON {
		raw, _ := json.MarshalIndent(entries, "", "  ")
		fmt.Println(string(raw))
		return 0
	}
	fmt.Printf("%-22s %-30s %8s %10s %10s\n", "ALIAS", "LABEL", "CTX", "$IN/M", "$OUT/M")
	for _, e := range entries {
		fmt.Printf("%-22s %-30s %8s %10.2f %10.2f\n",
			e.Alias, e.Label, fmtContext(e.Context), e.Input, e.Output)
	}
	return 0
}

func publicModels(cfg *config.Config) []publicModel {
	var out []publicModel
	for _, e := range cfg.Catalog {
		p := cfg.ModelPrice(e.Model)
		if e.Pricing != nil {
			p = *e.Pricing
		}
		out = append(out, publicModel{
			Alias: e.Alias, Label: firstNonEmptyStr(e.Label, e.Alias),
			Context: e.Context, Input: p.Input, Output: p.Output,
		})
	}
	if len(out) == 0 && cfg.Model != "" {
		p := cfg.ModelPrice(cfg.Model)
		alias := cfg.Model
		if i := strings.LastIndex(alias, "/"); i >= 0 {
			alias = alias[i+1:]
		}
		out = append(out, publicModel{
			Alias: alias, Label: alias, Input: p.Input, Output: p.Output,
		})
	}
	return out
}

func fmtContext(n int) string {
	if n <= 0 {
		return "-"
	}
	if n >= 1000 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprintf("%d", n)
}

type publicProvider struct {
	Provider string   `json:"provider"`
	Upstream string   `json:"upstream"`
	Env      []string `json:"env"`
	Status   string   `json:"status"`
}

func cmdProviders(args []string) int {
	fs := flag.NewFlagSet("providers", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = usageText
	var cfgPath string
	var asJSON bool
	fs.StringVar(&cfgPath, "config", "", "configuration file")
	fs.StringVar(&cfgPath, "c", "", "configuration file")
	fs.BoolVar(&asJSON, "json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cfg, err := loadConfig(cfgPath, "", 0)
	if err != nil {
		return fail(err)
	}
	applySecretFiles(cfg)
	names := make([]string, 0, len(cfg.Providers))
	for id := range cfg.Providers {
		names = append(names, id)
	}
	sort.Strings(names)
	views := make([]publicProvider, 0, len(names))
	for _, id := range names {
		p := cfg.Providers[id]
		status := "missing"
		for _, env := range p.Env {
			if os.Getenv(env) != "" {
				status = "configured"
				break
			}
		}
		views = append(views, publicProvider{Provider: id, Upstream: p.Upstream, Env: p.Env, Status: status})
	}
	if asJSON {
		raw, _ := json.MarshalIndent(views, "", "  ")
		fmt.Println(string(raw))
		return 0
	}
	fmt.Printf("%-14s %-42s %-34s %s\n", "PROVIDER", "UPSTREAM", "ENV", "STATUS")
	for _, v := range views {
		fmt.Printf("%-14s %-42s %-34s %s\n", v.Provider, v.Upstream, strings.Join(v.Env, ","), v.Status)
	}
	fmt.Println("\nSet keys through the environment, an env file, or Docker secrets:")
	fmt.Println("  docker run --env-file ./warden.env ...")
	fmt.Println("  docker run -e SILICONFLOW_API_KEY=... ...")
	fmt.Println("Never put key values in warden.yaml.")
	return 0
}

func firstNonEmptyStr(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

type doctorCheck struct {
	Check  string `json:"check"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = usageText
	var cfgPath string
	var asJSON bool
	fs.StringVar(&cfgPath, "config", "", "configuration file")
	fs.StringVar(&cfgPath, "c", "", "configuration file")
	fs.BoolVar(&asJSON, "json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cfg, err := loadConfig(cfgPath, "", 0)
	if err != nil {
		return fail(err)
	}
	applySecretFiles(cfg)

	var checks []doctorCheck
	status := "ok"
	add := func(name, st, detail string) {
		checks = append(checks, doctorCheck{name, st, detail})
		if st == "fail" {
			status = "fail"
		}
	}

	if _, err := exec.LookPath(cfg.EngineBin); err != nil {
		add("engine", "fail", "engine binary not found in PATH")
	} else {
		add("engine", "ok", "engine binary available")
	}

	dir := filepath.Dir(cfg.StateFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		add("state", "fail", "cannot create "+dir+": "+err.Error())
	} else {
		f, err := os.CreateTemp(dir, ".warden-check-*")
		if err != nil {
			add("state", "fail", "cannot write to "+dir)
		} else {
			f.Close()
			os.Remove(f.Name())
			add("state", "ok", cfg.StateFile)
		}
	}

	if cfg.GatewayEnabled() {
		if len(cfg.Providers) == 0 {
			add("gateway", "warn", "enabled but no providers configured; requests stay direct")
		} else {
			add("gateway", "ok", fmt.Sprintf("%d provider(s) routed through local proxy", len(cfg.Providers)))
		}
	} else {
		add("gateway", "warn", "disabled; rate limits apply per run, not per request")
	}

	names := make([]string, 0, len(cfg.Providers))
	for id := range cfg.Providers {
		names = append(names, id)
	}
	sort.Strings(names)
	for _, id := range names {
		p := cfg.Providers[id]
		if p.Upstream == "" {
			add("provider/"+id, "fail", "missing upstream")
			continue
		}
		found := false
		for _, env := range p.Env {
			if v := os.Getenv(env); v != "" {
				found = true
				break
			}
		}
		if found {
			add("provider/"+id, "ok", p.Upstream)
		} else {
			add("provider/"+id, "warn", "missing credentials: "+strings.Join(p.Env, ", "))
		}
	}

	seen := map[string]bool{}
	for _, e := range cfg.Catalog {
		if e.Alias == "" || e.Model == "" {
			add("catalog", "fail", "entry with empty alias or model")
			continue
		}
		if seen[e.Alias] {
			add("catalog/"+e.Alias, "fail", "duplicate alias")
		}
		seen[e.Alias] = true
	}
	if len(cfg.Catalog) > 0 {
		add("catalog", "ok", fmt.Sprintf("%d models exposed", len(cfg.Catalog)))
	}

	if asJSON {
		raw, _ := json.MarshalIndent(checks, "", "  ")
		fmt.Println(string(raw))
	} else {
		for _, c := range checks {
			fmt.Printf("%-5s %-20s %s\n", strings.ToUpper(c.Status), c.Check, c.Detail)
		}
	}
	if status == "fail" {
		return 1
	}
	return 0
}

// applySecretFiles resolves <ENV>_FILE variables (Docker secrets) into the
// environment used by the engine and gateway. Key values are never logged.
func applySecretFiles(cfg *config.Config) {
	names := map[string]bool{}
	for _, p := range cfg.Providers {
		for _, env := range p.Env {
			names[env] = true
		}
	}
	for _, env := range []string{
		"OPENCODE_API_KEY", "SILICONFLOW_API_KEY", "DEEPSEEK_API_KEY",
		"GEMINI_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY", "GOOGLE_API_KEY",
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY",
	} {
		names[env] = true
	}
	for name := range names {
		if os.Getenv(name) != "" {
			continue
		}
		path := os.Getenv(name + "_FILE")
		if path == "" {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if v := strings.TrimSpace(string(raw)); v != "" {
			os.Setenv(name, v)
		}
	}
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}
