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
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/esdras-schonevald/open-warden/internal/config"
	"github.com/esdras-schonevald/open-warden/internal/gateway"
	"github.com/esdras-schonevald/open-warden/internal/registry"
	"github.com/esdras-schonevald/open-warden/internal/report"
	"github.com/esdras-schonevald/open-warden/internal/state"
	"github.com/esdras-schonevald/open-warden/internal/usage"
	"github.com/esdras-schonevald/open-warden/internal/warden"
)

var version = "0.6.0"

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
  open-warden models [--all]        List available models (alias, context, price)
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
      --base-url <url>       one-off OpenAI-compatible endpoint (requires -m)
      --api-key <key>        override the provider credential
      --api-key-file <file>  read the credential from a file
      --economy <mode>       token economy: off|balanced|aggressive
      --no-economy           disable the economy mode
      --subagents-model <m>  model alias used by subagents
      --subagents-fallback <m>  subagent fallback model alias
      --report-file <file>   write the run report as JSON to a file
      --json                 machine-readable output (includes full report)
  -q, --quiet                do not stream assistant text

Prompt is taken from arguments, --prompt-file, or stdin (when piped).

Exit codes: 0 ok, 1 failure, 2 usage, 3 budget reached.

Examples:
  open-warden models
  open-warden models --all --json
  open-warden providers
  open-warden run -m qwen-coder-30b "Fix the failing tests"
  open-warden run --base-url https://api.deepseek.com --api-key "$KEY" \
    -m deepseek-flash "Fix the failing tests"
  open-warden agents -c warden.yaml --budget 2.00 "Implement TASK.md"
  open-warden report -c warden.yaml --last 20
`)
}

type runFlags struct {
	config      string
	agent       string
	model       string
	fallback    string
	budget      float64
	workdir     string
	promptFile  string
	jsonOut     bool
	quiet       bool
	timeout     time.Duration
	baseURL     string
	apiKey      string
	apiKeyFile  string
	economy     string
	noEconomy   bool
	subModel    string
	subFallback string
	reportFile  string
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
	fs.StringVar(&rf.baseURL, "base-url", "", "one-off OpenAI-compatible endpoint")
	fs.StringVar(&rf.apiKey, "api-key", "", "API key override (or WARDEN_API_KEY)")
	fs.StringVar(&rf.apiKeyFile, "api-key-file", "", "read API key from file")
	fs.StringVar(&rf.economy, "economy", "", "economy mode: off|balanced|aggressive")
	fs.BoolVar(&rf.noEconomy, "no-economy", false, "disable economy mode")
	fs.StringVar(&rf.subModel, "subagents-model", "", "model alias used by subagents")
	fs.StringVar(&rf.subFallback, "subagents-fallback", "", "subagent fallback model alias")
	fs.StringVar(&rf.reportFile, "report-file", "", "write the run report as JSON to this file")
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
	applySecretFiles()
	if rf.baseURL != "" {
		if rf.model == "" && cfg.Model == "" {
			return fail(fmt.Errorf("--base-url requires -m <model>"))
		}
		if u, err := url.Parse(rf.baseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fail(fmt.Errorf("invalid --base-url: %s", rf.baseURL))
		}
	}
	if rf.model == "" && cfg.Model == "" {
		return fail(fmt.Errorf("no model selected: pass -m <alias> or configure model"))
	}
	apiKey, err := resolveAPIKey(rf)
	if err != nil {
		return fail(err)
	}
	if rf.fallback != "" {
		cfg.Fallbacks = append([]string{rf.fallback}, cfg.Fallbacks...)
	}
	if rf.timeout > 0 {
		cfg.Run.Timeout = rf.timeout
	}
	if rf.subModel != "" {
		cfg.Subagents.Model = rf.subModel
	}
	if rf.subFallback != "" {
		cfg.Subagents.FallbackModel = rf.subFallback
	}
	if rf.noEconomy && rf.economy != "" {
		return fail(fmt.Errorf("use either --economy or --no-economy"))
	}
	if rf.noEconomy {
		cfg.Economy.Mode = "off"
	} else if rf.economy != "" {
		switch rf.economy {
		case "off", "balanced", "aggressive":
		default:
			return fail(fmt.Errorf("invalid --economy mode %q (use off, balanced or aggressive)", rf.economy))
		}
		cfg.Economy = config.Economy{Mode: rf.economy}
		cfg.Economy.Defaults()
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
	if rf.baseURL != "" {
		model := rf.model
		if model == "" {
			model = cfg.Model
		}
		if m, ok := registry.Resolve(model); ok {
			model = m.EngineModel
		} else if i := strings.Index(model, "/"); i > 0 {
			if _, known := registry.GetProvider(model[:i]); known {
				model = model[i+1:]
			}
		}
		runner.Custom = &warden.CustomRun{BaseURL: rf.baseURL, Model: model, APIKey: apiKey}
	} else if apiKey != "" {
		runner.APIKey = apiKey
		selected := rf.model
		if selected == "" {
			selected = cfg.Model
		}
		if m, ok := registry.Resolve(selected); ok {
			runner.APIKeyProvider = m.Provider
		} else if i := strings.Index(selected, "/"); i > 0 {
			runner.APIKeyProvider = selected[:i]
		}
	}

	providers := map[string]registry.Provider{}
	for _, p := range registry.Providers() {
		if p.Protocol == registry.Free || p.Upstream == "" {
			continue
		}
		providers[p.ID] = p
	}
	if runner.Custom != nil {
		providers["custom"] = registry.Provider{
			ID: "custom", Name: "Custom", Upstream: runner.Custom.BaseURL,
			Env: []string{"WARDEN_CUSTOM_API_KEY"}, Protocol: registry.OpenAI,
		}
	}
	gw, err := gateway.New(providers, runner.Lim, gateway.Options{
		MaxWait:   20 * time.Second,
		Headroom:  cfg.Run.OutputHeadroom,
		MaxOutput: cfg.Economy.OutputCap(),
		MaxInput:  cfg.Economy.MaxInputTokens,
	})
	if err != nil {
		return fail(err)
	}
	defer gw.Close()
	runner.GatewayURL = gw.URL
	runner.GatewayToken = gw.Token
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
		cappedBefore, truncatedBefore := gw.Capped(), gw.Truncated()
		rep, err := runner.Run(ctx, opts)
		if rep != nil {
			rep.OutputCapApplied = gw.Capped() - cappedBefore
			rep.OutputCapTokens = cfg.Economy.OutputCap()
			rep.OutputTruncated = gw.Truncated() - truncatedBefore
		}
		if rf.reportFile != "" && rep != nil {
			if werr := writeReport(rf.reportFile, rep); werr != nil {
				fmt.Fprintln(os.Stderr, "error: write report:", werr)
			}
		}
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

func writeReport(path string, rep *report.Run) error {
	raw, err := rep.JSON()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(raw+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
	if path != "" {
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) && path == os.Getenv("WARDEN_CONFIG") {
			// The image sets WARDEN_CONFIG by default; a missing mount should
			// fall back to defaults instead of blocking ad-hoc runs.
			path = ""
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
		fmt.Printf("%-22s %-12s %-18s %9d %9d %8d %10.6f %9.1fs  %s\n",
			short(r.ID), r.Agent, r.Status, r.Input, r.Output, r.Reasoning, r.Cost, r.DurationSec,
			registry.PublicName(r.Model))
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
	if !cfg.HasModel() {
		return fail(fmt.Errorf("no model configured: set model in config"))
	}
	fmt.Printf("config: %s\n", coalesce(cfgPath, "defaults"))
	fmt.Printf("model:  %s\n", cfg.Model)
	fmt.Printf("fallbacks: %s\n", strings.Join(cfg.Fallbacks, " -> "))
	fmt.Printf("budget/run: $%.2f\n", cfg.Budget)
	for _, name := range append([]string{cfg.Model}, cfg.Fallbacks...) {
		ref := resolveModelRef(name)
		l := registry.LimitsFor(ref)
		p := registry.PriceFor(ref)
		fmt.Printf("  %-45s rpm=%d tpm=%d rpd=%d tpd=%d  $%.2f/$%.2f per 1M\n",
			name, l.RPM, l.TPM, l.RPD, l.TPD, p.Input, p.Output)
	}
	fmt.Printf("subagents: model=%s fallback=%s\n", coalesce(cfg.Subagents.Model, "-"), coalesce(cfg.Subagents.FallbackModel, "-"))
	fmt.Printf("economy: mode=%s output_cap=%d input_cap=%d\n",
		cfg.Economy.Mode, cfg.Economy.OutputCap(), cfg.Economy.MaxInputTokens)
	fmt.Printf("gateway: always on (free-tier direct)\n")
	fmt.Printf("registry: %d providers, %d models\n", len(registry.Providers()), len(registry.Models()))
	agents := (&warden.Runner{Cfg: cfg}).AgentNames("")
	fmt.Printf("agents: %s\n", strings.Join(agents, ", "))
	return 0
}

func resolveModelRef(name string) string {
	if m, ok := registry.Resolve(name); ok {
		return m.Ref()
	}
	return name
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
	var asJSON, all bool
	fs.BoolVar(&asJSON, "json", false, "JSON output")
	fs.BoolVar(&all, "all", false, "include models whose provider key is not set")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	entries := publicModels(all)
	if len(fs.Args()) > 0 {
		want := fs.Args()[0]
		for _, m := range registry.Models() {
			if m.Alias != want {
				continue
			}
			e := toPublicModel(m)
			if asJSON {
				raw, _ := json.MarshalIndent(e, "", "  ")
				fmt.Println(string(raw))
				return 0
			}
			fmt.Printf("alias:   %s\nlabel:   %s\ncontext: %s\ninput:   $%.2f/M\noutput:  $%.2f/M\n",
				e.Alias, e.Label, fmtContext(e.Context), e.Input, e.Output)
			return 0
		}
		return fail(fmt.Errorf("unknown model alias %q", want))
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

func publicModels(all bool) []publicModel {
	src := registry.AvailableModels()
	if all {
		src = registry.Models()
	}
	out := make([]publicModel, 0, len(src))
	for _, m := range src {
		out = append(out, toPublicModel(m))
	}
	return out
}

func toPublicModel(m registry.Model) publicModel {
	return publicModel{
		Alias: m.Alias, Label: m.Label, Context: m.Context,
		Input: m.Price.Input, Output: m.Price.Output,
	}
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
	KeyURL   string   `json:"key_url,omitempty"`
}

func cmdProviders(args []string) int {
	fs := flag.NewFlagSet("providers", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = usageText
	var asJSON bool
	fs.BoolVar(&asJSON, "json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	applySecretFiles()
	var views []publicProvider
	for _, p := range registry.Providers() {
		status := "missing"
		if p.Protocol == registry.Free {
			status = "managed"
		} else {
			for _, env := range p.Env {
				if os.Getenv(env) != "" {
					status = "configured"
					break
				}
			}
		}
		views = append(views, publicProvider{
			Provider: p.ID, Upstream: coalesce(p.Upstream, "managed"),
			Env: p.Env, Status: status, KeyURL: p.KeyURL,
		})
	}
	if asJSON {
		raw, _ := json.MarshalIndent(views, "", "  ")
		fmt.Println(string(raw))
		return 0
	}
	fmt.Printf("%-14s %-38s %-32s %s\n", "PROVIDER", "UPSTREAM", "ENV", "STATUS")
	for _, v := range views {
		fmt.Printf("%-14s %-38s %-32s %s\n", v.Provider, v.Upstream, strings.Join(v.Env, ","), v.Status)
	}
	fmt.Println("\nSet keys through the environment, an env file, or Docker secrets:")
	fmt.Println("  docker run --env-file ./warden.env ...")
	fmt.Println("  docker run -e SILICONFLOW_API_KEY=... ...")
	fmt.Println("  docker run -e SILICONFLOW_API_KEY_FILE=/run/secrets/siliconflow ...")
	fmt.Println("Never put key values in warden.yaml.")
	return 0
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
	applySecretFiles()

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

	routed := 0
	for _, p := range registry.Providers() {
		if p.Protocol == registry.Free || p.Upstream == "" {
			continue
		}
		routed++
	}
	add("gateway", "ok", fmt.Sprintf("always on; %d provider(s) routed, free-tier direct", routed))

	for _, p := range registry.Providers() {
		if p.Protocol == registry.Free {
			add("provider/"+p.ID, "ok", "managed, no key required")
			continue
		}
		found := false
		for _, env := range p.Env {
			if os.Getenv(env) != "" {
				found = true
				break
			}
		}
		if found {
			add("provider/"+p.ID, "ok", p.Upstream)
		} else {
			add("provider/"+p.ID, "warn", "missing credentials: "+strings.Join(p.Env, ", "))
		}
	}

	seen := map[string]bool{}
	for _, m := range registry.Models() {
		if m.Alias == "" || m.Ref() == "" {
			add("registry", "fail", "model entry with empty alias or reference")
			continue
		}
		if seen[m.Alias] {
			add("registry/"+m.Alias, "fail", "duplicate alias")
		}
		seen[m.Alias] = true
	}
	add("registry", "ok", fmt.Sprintf("%d providers, %d models", len(registry.Providers()), len(registry.Models())))

	if !cfg.HasModel() {
		add("model", "warn", "no model selected in config; pass -m at run time")
	} else if _, ok := registry.Resolve(cfg.Model); ok {
		add("model", "ok", cfg.Model)
	} else {
		add("model", "warn", "model is not in the built-in registry: "+cfg.Model)
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

func coalesce(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// applySecretFiles resolves <ENV>_FILE variables (Docker secrets) into the
// environment used by the engine and gateway. Key values are never logged.
func applySecretFiles() {
	names := map[string]bool{}
	for _, p := range registry.Providers() {
		for _, env := range p.Env {
			names[env] = true
		}
	}
	names["WARDEN_API_KEY"] = true
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

func resolveAPIKey(rf *runFlags) (string, error) {
	if rf.apiKey != "" && rf.apiKeyFile != "" {
		return "", fmt.Errorf("use either --api-key or --api-key-file")
	}
	if rf.apiKeyFile != "" {
		raw, err := os.ReadFile(rf.apiKeyFile)
		if err != nil {
			return "", fmt.Errorf("read api key file: %w", err)
		}
		return strings.TrimSpace(string(raw)), nil
	}
	if rf.apiKey != "" {
		return rf.apiKey, nil
	}
	return strings.TrimSpace(os.Getenv("WARDEN_API_KEY")), nil
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}
