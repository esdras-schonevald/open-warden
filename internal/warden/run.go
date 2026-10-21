// Package warden orchestrates runs: proactive admission, fallback routing,
// budget enforcement, parallel agents and reporting.
package warden

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/esdras-schonevald/open-warden/internal/config"
	"github.com/esdras-schonevald/open-warden/internal/engine"
	"github.com/esdras-schonevald/open-warden/internal/limiter"
	"github.com/esdras-schonevald/open-warden/internal/report"
	"github.com/esdras-schonevald/open-warden/internal/state"
	"github.com/esdras-schonevald/open-warden/internal/usage"
)

// Options override config values for one run.
type Options struct {
	Agent    string
	Prompt   string
	Model    string
	Fallback string
	Budget   float64
	Workdir  string
	JSON     bool
	Quiet    bool
}

// Runner executes warden runs.
type Runner struct {
	Cfg   *config.Config
	Lim   *limiter.Limiter
	State *state.File
	Out   io.Writer
	// Collect reads usage for a session tree; injectable for tests.
	Collect func(root string, since time.Time) (*usage.Report, error)
}

// New builds a runner from config, loading persistent state.
func New(cfg *config.Config) (*Runner, error) {
	st, err := state.Load(cfg.StateFile)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	return &Runner{
		Cfg:     cfg,
		Lim:     limiter.New(cfg.Limits, st.Windows),
		State:   st,
		Out:     os.Stdout,
		Collect: usage.Collect,
	}, nil
}

// persist saves windows and run history, merging with any concurrent writer.
func (r *Runner) persist(rec *state.RunRecord) error {
	windows := r.Lim.Snapshot()
	return state.Update(r.Cfg.StateFile, func(f *state.File) {
		f.Windows = windows
		if rec != nil {
			f.History = append(f.History, *rec)
			if len(f.History) > 500 {
				f.History = f.History[len(f.History)-500:]
			}
		}
	})
}

// sessionSet tracks the sessions of a run attempt atomically, since the
// budget poller reads them while the runner appends new attempts.
type sessionSet struct {
	mu  sync.Mutex
	ids []string
}

func (s *sessionSet) Add(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids = append(s.ids, id)
	return len(s.ids) - 1
}

func (s *sessionSet) Set(i int, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != "" && i >= 0 && i < len(s.ids) {
		s.ids[i] = id
	}
}

func (s *sessionSet) Get() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.ids))
	copy(out, s.ids)
	return out
}

// composePrompt merges global, agent-specific and task prompts.
func (r *Runner) composePrompt(agent string, task string) (string, error) {
	var parts []string
	global, err := r.Cfg.GlobalPrompt()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(global) != "" {
		parts = append(parts, strings.TrimSpace(global))
	}
	if spec, ok := r.Cfg.Agent(agent); ok {
		body := spec.Prompt
		if spec.PromptFile != "" {
			raw, err := os.ReadFile(spec.PromptFile)
			if err != nil {
				return "", fmt.Errorf("read agent prompt for %q: %w", agent, err)
			}
			body = string(raw)
		}
		if strings.TrimSpace(body) != "" {
			parts = append(parts, strings.TrimSpace(body))
		}
	}
	if strings.TrimSpace(task) != "" {
		parts = append(parts, strings.TrimSpace(task))
	}
	return strings.Join(parts, "\n\n"), nil
}

// chain returns the ordered models to attempt, honoring per-agent override.
func (r *Runner) chain(opts Options, spec config.AgentSpec) []string {
	primary := firstNonEmpty(opts.Model, spec.Model, r.Cfg.Model)
	var chain []string
	seen := map[string]bool{}
	add := func(m string) {
		m = strings.TrimSpace(m)
		if m != "" && !seen[m] {
			seen[m] = true
			chain = append(chain, m)
		}
	}
	add(primary)
	add(spec.Fallback)
	if opts.Fallback != "" {
		add(opts.Fallback)
	}
	for _, m := range r.Cfg.Fallbacks {
		add(m)
	}
	return chain
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// estimateTokens predicts the input weight of a request for TPM admission.
func (r *Runner) estimateTokens(prompt string) int {
	div := r.Cfg.Run.EstDivisor
	if div <= 0 {
		div = 4
	}
	return int(float64(len(prompt))/div) + r.Cfg.Run.OutputHeadroom
}

// admission picks the first model in the chain the limiter admits right now.
// It returns the chosen model, its index, and an optional wait duration.
func (r *Runner) admission(chain []string, est int) (string, int, time.Duration, error) {
	for i, m := range chain {
		if ok := r.Lim.Admit(m, est); ok.Allowed {
			return m, i, 0, nil
		}
	}
	wait := r.Lim.EarliestWait(chain, est)
	if r.Cfg.Fallback.WaitBlocked {
		return "", -1, wait, nil
	}
	return "", -1, 0, fmt.Errorf("all models are rate-limited (nearest window frees in %s)", wait.Round(time.Second))
}

// Run executes one agent run with proactive fallback and budget enforcement.
func (r *Runner) Run(ctx context.Context, opts Options) (*report.Run, error) {
	spec, _ := r.Cfg.Agent(opts.Agent)
	prompt, err := r.composePrompt(opts.Agent, opts.Prompt)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("empty prompt: pass a prompt argument, --prompt-file or configure prompt.global")
	}
	chain := r.chain(opts, spec)
	if len(chain) == 0 {
		return nil, errors.New("no model configured")
	}
	workdir := firstNonEmpty(opts.Workdir, spec.Workdir, ".")
	budget := opts.Budget
	if budget <= 0 {
		budget = r.Cfg.Budget
	}
	est := r.estimateTokens(prompt)

	rep := &report.Run{
		ID:        engine.NewSessionID(),
		Agent:     firstNonEmpty(opts.Agent, "default"),
		Started:   time.Now().UTC(),
		Model:     chain[0],
		Budget:    budget,
		Status:    "running",
		Fallbacks: []report.FallbackEvent{},
	}
	set := &sessionSet{}
	currentModel := chain[0]

	attempt := 0
	var lastErr error
	var lastClass engine.ErrClass

	for attempt < r.Cfg.Fallback.MaxRetries && attempt < len(chain)*2 {
		if ctx.Err() != nil {
			rep.Status = "canceled"
			break
		}
		model, idx, wait, err := r.admission(chain[attempt%len(chain):], est)
		if err != nil {
			lastErr = err
			lastClass = engine.ErrRateLimit
			break
		}
		if wait > 0 {
			// every window is closed; wait for the nearest opening
			rep.RateWaits++
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				rep.Status = "canceled"
			}
			continue
		}
		if wait == 0 && model == "" {
			lastErr = fmt.Errorf("all models are rate-limited")
			lastClass = engine.ErrRateLimit
			break
		}
		if idx > 0 {
			rep.Fallbacks = append(rep.Fallbacks, report.FallbackEvent{
				At: time.Now().UTC(), From: currentModel, To: model,
				Reason: string(engine.ErrRateLimit), Scope: "main",
			})
			if r.Cfg.Fallback.Subagents {
				// keep subagents on a model that is not exhausted
				rep.Fallbacks = append(rep.Fallbacks, report.FallbackEvent{
					At: time.Now().UTC(), From: r.subagentModel(currentModel), To: r.subagentModel(model),
					Reason: string(engine.ErrRateLimit), Scope: "subagents",
				})
			}
		}
		reservedAt := r.Lim.Consume(model, est)
		rep.Model = model
		currentModel = model

		pol := engine.SubagentPolicy{
			Enabled:     r.subagentEnabled(),
			Model:       r.subagentModel(model),
			Instruction: r.subagentInstruction(),
		}
		env, cleanup, err := engine.PrepareSubagents(pol)
		if err != nil {
			return nil, fmt.Errorf("prepare subagent policy: %w", err)
		}
		sessionID := engine.NewSessionID()
		slot := set.Add(sessionID)

		runCtx, cancel := context.WithCancel(ctx)
		if r.Cfg.Run.Timeout > 0 {
			runCtx, cancel = context.WithTimeout(ctx, r.Cfg.Run.Timeout)
		}
		budgetHit := r.watchBudget(runCtx, cancel, set, budget)
		if !opts.JSON {
			fmt.Fprintf(r.Out, "\n")
		}
		var text strings.Builder
		res, class := engine.Run(runCtx, engine.Options{
			Binary:      r.Cfg.EngineBin,
			Workdir:     workdir,
			Model:       model,
			SessionID:   sessionID,
			Prompt:      prompt,
			AutoApprove: r.Cfg.Run.Auto(),
			Standalone:  r.Cfg.Standalone,
			Subagents:   pol,
			ExtraEnv:    env,
		}, func(ev engine.Event) {
			if ev.SessionID != "" {
				// discover the engine-created session so the budget poller
				// can track cost while the run streams
				set.Set(slot, ev.SessionID)
			}
			if ev.Part.Type != "text" {
				return
			}
			text.WriteString(ev.Part.Text)
			if opts.JSON || opts.Quiet {
				return
			}
			fmt.Fprint(r.Out, engine.Sanitize(ev.Part.Text))
		})
		cancel()
		cleanup()
		rep.Output = engine.Sanitize(text.String())
		if res != nil && res.SessionID != "" {
			set.Set(slot, res.SessionID)
			rep.ID = res.SessionID
		}
		// reconcile the TPM estimate with what the request actually consumed
		if actual, aerr := r.Collect(set.Get()[slot], rep.Started); aerr == nil && actual != nil {
			used := actual.Total.Input + actual.Total.Output +
				actual.Total.Reasoning + actual.Total.CacheRead
			r.Lim.Adjust(model, reservedAt, used)
		}
		if budgetHit() {
			rep.BudgetExceeded = true
			rep.Status = "budget_exceeded"
			lastErr = fmt.Errorf("budget of $%.2f reached", budget)
			lastClass = engine.ErrBudget
			break
		}
		if class == engine.ErrNone && res != nil && res.ExitCode == 0 {
			rep.Status = "ok"
			lastErr = nil
			lastClass = engine.ErrNone
			break
		}
		lastClass = class
		detail := ""
		if res != nil {
			detail = strings.TrimSpace(res.Stderr)
		}
		if detail == "" {
			detail = fmt.Sprintf("engine exited with code %d", res.ExitCode)
		}
		detail = engine.Sanitize(detail)
		lastErr = fmt.Errorf("%s", detail)
		if !r.shouldFallback(class) {
			rep.Status = string(class)
			break
		}
		next := chain[(attempt+1)%len(chain)]
		if next == model {
			rep.Status = string(class)
			break
		}
		rep.Fallbacks = append(rep.Fallbacks, report.FallbackEvent{
			At: time.Now().UTC(), From: model, To: next,
			Reason: string(class), Scope: "main",
		})
		attempt++
		if attempt >= r.Cfg.Fallback.MaxRetries {
			rep.Status = string(class)
		}
	}

	rep.DurationSec = time.Since(rep.Started).Seconds()
	rep.Usage = r.collectAll(set.Get(), rep.Started)
	rep.Cost = rep.Usage.Total.Cost
	if rep.Status == "running" {
		rep.Status = "ok"
	}
	if lastErr != nil && rep.Status != "ok" && rep.Status != "budget_exceeded" {
		rep.ErrorDetail = engine.Sanitize(lastErr.Error())
	}
	if lastClass == engine.ErrBudget {
		rep.Status = "budget_exceeded"
	}
	if len(rep.Fallbacks) > 0 {
		rep.Status = statusWithFallback(rep.Status)
	}

	rec := &state.RunRecord{
		ID: rep.ID, Agent: rep.Agent, Model: rep.Model, Started: rep.Started,
		DurationSec: rep.DurationSec, Cost: rep.Cost, Budget: rep.Budget,
		BudgetExceeded: rep.BudgetExceeded, Status: rep.Status,
		Requests:      rep.Usage.Total.Requests,
		Input:         rep.Usage.Total.Input,
		Output:        rep.Usage.Total.Output,
		Reasoning:     rep.Usage.Total.Reasoning,
		CacheRead:     rep.Usage.Total.CacheRead,
		CacheWrite:    rep.Usage.Total.CacheWrite,
		SubInput:      rep.Usage.Subagents.Input,
		SubOutput:     rep.Usage.Subagents.Output,
		SubReasoning:  rep.Usage.Subagents.Reasoning,
		SubCacheRead:  rep.Usage.Subagents.CacheRead,
		SubCacheWrite: rep.Usage.Subagents.CacheWrite,
		RateWaits:     rep.RateWaits,
	}
	for _, f := range rep.Fallbacks {
		rec.Fallbacks = append(rec.Fallbacks, f.From+"->"+f.To+":"+f.Reason+":"+f.Scope)
	}
	if err := r.persist(rec); err != nil {
		return rep, fmt.Errorf("persist state: %w", err)
	}
	if lastErr != nil && rep.Status != "ok" && rep.Status != "budget_exceeded" {
		return rep, lastErr
	}
	return rep, nil
}

func statusWithFallback(s string) string {
	if s == "ok" {
		return "ok_with_fallback"
	}
	return s
}

func (r *Runner) shouldFallback(class engine.ErrClass) bool {
	if class == engine.ErrNone || class == engine.ErrBudget {
		return false
	}
	for _, t := range r.Cfg.Fallback.On {
		if string(t) == string(class) {
			return true
		}
	}
	return class == engine.ErrRateLimit || class == engine.ErrQuota || class == engine.ErrUnavailable
}

func (r *Runner) subagentModel(mainModel string) string {
	base := r.Cfg.Subagents.Model
	if base == "" {
		base = mainModel
	}
	// when the main model had to fall back, move subagents as well
	if mainModel != "" && mainModel != r.Cfg.Model && r.Cfg.Subagents.FallbackModel != "" {
		return r.Cfg.Subagents.FallbackModel
	}
	return base
}

func (r *Runner) subagentEnabled() bool {
	s := r.Cfg.Subagents
	return s.Model != "" || s.FallbackModel != "" || s.Instruction != "" || s.InstructionFile != ""
}

func (r *Runner) subagentInstruction() string {
	if r.Cfg.Subagents.InstructionFile != "" {
		if raw, err := os.ReadFile(r.Cfg.Subagents.InstructionFile); err == nil {
			return string(raw)
		}
	}
	return r.Cfg.Subagents.Instruction
}

// collect merges usage across all attempted sessions.
func (r *Runner) collectAll(sessions []string, since time.Time) *usage.Report {
	out := &usage.Report{ByModel: map[string]usage.Totals{}}
	for _, sid := range sessions {
		rep, err := r.Collect(sid, since)
		if err != nil || rep == nil {
			continue
		}
		out.Root.Add(rep.Root)
		out.Subagents.Add(rep.Subagents)
		out.Total.Add(rep.Total)
		for m, t := range rep.ByModel {
			cur := out.ByModel[m]
			cur.Add(t)
			out.ByModel[m] = cur
		}
	}
	r.applyPricingFloor(out)
	return out
}

// applyPricingFloor uses the configured price table as a lower bound for cost.
// The engine's provider-reported cost wins when present; this guarantees the
// budget still works for providers/models the engine prices at zero.
func (r *Runner) applyPricingFloor(rep *usage.Report) {
	if len(r.Cfg.Pricing) == 0 {
		return
	}
	estimatedTotal := 0.0
	for model, t := range rep.ByModel {
		p := r.Cfg.ModelPrice(model)
		reasoning := p.Reasoning
		if reasoning == 0 {
			reasoning = p.Output
		}
		est := float64(t.Input)*p.Input/1e6 +
			float64(t.Output)*p.Output/1e6 +
			float64(t.Reasoning)*reasoning/1e6 +
			float64(t.CacheRead)*p.CacheRead/1e6 +
			float64(t.CacheWrite)*p.CacheWrite/1e6
		if est > t.Cost {
			t.Cost = est
			rep.ByModel[model] = t
		}
		estimatedTotal += t.Cost
	}
	if estimatedTotal > rep.Total.Cost {
		rep.Total.Cost = estimatedTotal
	}
}

// watchBudget polls cost while the run streams and cancels it at the limit.
// It returns a function reporting whether the budget triggered.
func (r *Runner) watchBudget(ctx context.Context, cancel context.CancelFunc, set *sessionSet, budget float64) func() bool {
	if budget <= 0 {
		return func() bool { return false }
	}
	var mu sync.Mutex
	hit := false
	interval := r.Cfg.Run.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				total := 0.0
				for _, sid := range set.Get() {
					if rep, err := r.Collect(sid, time.Time{}); err == nil && rep != nil {
						total += rep.Total.Cost
					}
				}
				if total >= budget {
					mu.Lock()
					hit = true
					mu.Unlock()
					cancel()
					return
				}
			}
		}
	}()
	return func() bool {
		mu.Lock()
		defer mu.Unlock()
		return hit
	}
}
