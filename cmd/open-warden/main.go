// Command open-warden is a CLI that runs guarded, budgeted agents on top of an
// embedded inference engine, with proactive rate limiting and fallback routing.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/esdras-schonevald/open-warden/internal/config"
	"github.com/esdras-schonevald/open-warden/internal/report"
	"github.com/esdras-schonevald/open-warden/internal/state"
	"github.com/esdras-schonevald/open-warden/internal/usage"
	"github.com/esdras-schonevald/open-warden/internal/warden"
)

const version = "0.1.0"

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
  open-warden report                Show token/cost report from run history
  open-warden validate              Validate configuration and show effective policy
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
	rf := parseRunFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfig(rf.config, rf.model, rf.budget)
	if err != nil {
		return fail(err)
	}
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
	var cfgPath, statePath string
	var asJSON bool
	var last int
	fs.StringVar(&cfgPath, "config", "", "configuration file")
	fs.StringVar(&cfgPath, "c", "", "configuration file")
	fs.StringVar(&statePath, "state", "", "state file")
	fs.BoolVar(&asJSON, "json", false, "JSON output")
	fs.IntVar(&last, "last", 20, "number of recent runs")
	if err := fs.Parse(args); err != nil {
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
	var cfgPath string
	fs.StringVar(&cfgPath, "config", "", "configuration file")
	fs.StringVar(&cfgPath, "c", "", "configuration file")
	if err := fs.Parse(args); err != nil {
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

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}
