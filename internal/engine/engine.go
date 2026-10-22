// Package engine wraps the internal inference engine binary.
//
// The engine is an implementation detail of open-warden and is never surfaced
// to end users: process output is sanitized and errors are reported in
// warden's own vocabulary (rate_limit, quota, unavailable, budget...).
package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/esdras-schonevald/open-warden/internal/config"
)

// Event is one streamed event from a run.
type Event struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Error     *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	Part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"part"`
}

// SessionRef identifies the run session.
type SessionRef struct {
	ID string
}

// SubagentPolicy controls the models and instruction assigned to subagents.
type SubagentPolicy struct {
	Model    string
	Fallback string
	Enabled  bool
}

// Options for a single engine run.
type Options struct {
	Binary      string
	Workdir     string
	Model       string
	SessionID   string
	Prompt      string
	AutoApprove bool
	Standalone  bool
	Subagents   SubagentPolicy
	ExtraEnv    []string
}

// Result carries raw process outcome.
type Result struct {
	ExitCode  int
	Stderr    string
	Stdout    string
	SessionID string
	Duration  time.Duration
}

// ErrClass is a sanitized failure category.
type ErrClass string

const (
	ErrRateLimit   ErrClass = "rate_limit"
	ErrQuota       ErrClass = "quota"
	ErrUnavailable ErrClass = "unavailable"
	ErrTimeout     ErrClass = "timeout"
	ErrBudget      ErrClass = "budget_exceeded"
	ErrNone        ErrClass = ""
)

var rateMarkers = []string{"429", "rate limit", "rate_limit", "ratelimit", "too many requests", "rate-limited"}
var quotaMarkers = []string{"insufficient", "insufficient_balance", "balance", "credit", "quota", "billing", "payment", "402"}

// Classify maps raw stderr/exit code to a sanitized error class.
func Classify(stderr string, exit int, timedOut bool) ErrClass {
	low := strings.ToLower(stderr)
	if timedOut {
		return ErrTimeout
	}
	for _, m := range rateMarkers {
		if strings.Contains(low, m) {
			return ErrRateLimit
		}
	}
	for _, m := range quotaMarkers {
		if strings.Contains(low, m) {
			return ErrQuota
		}
	}
	if exit != 0 {
		return ErrUnavailable
	}
	return ErrNone
}

// Sanitize removes engine-specific branding from user-facing text.
func Sanitize(s string) string {
	s = strings.ReplaceAll(s, "opencode", "engine")
	s = strings.ReplaceAll(s, "OpenCode", "engine")
	s = strings.ReplaceAll(s, "OPENCODE", "ENGINE")
	return s
}

// NewSessionID returns a fresh unique session identifier.
func NewSessionID() string {
	buf := make([]byte, 16)
	if _, err := readRandom(buf); err != nil {
		return fmt.Sprintf("ses_%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("ses_%x", buf)
}

func readRandom(b []byte) (int, error) {
	f, err := os.Open("/dev/urandom")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Read(b)
}

// agentNames enumerates the built-in subagent roles whose models and
// instructions the warden policy overrides.
var agentNames = []string{"explore", "general", "plan"}

// ProviderOverride points one provider at warden's local gateway.
type ProviderOverride struct {
	BaseURL string
	Headers map[string]string
	Name    string
	Package string
	Env     []string
	Models  []string
}

// ConfigOptions describes the scoped engine configuration generated per run.
type ConfigOptions struct {
	Subagents SubagentPolicy
	Providers map[string]ProviderOverride
	Economy   *config.Economy
}

// PrepareConfig writes a scoped engine config (subagent policy and provider
// gateway overrides) and returns env vars plus a cleanup function.
func PrepareConfig(opts ConfigOptions) ([]string, func(), error) {
	noop := func() {}
	cfg := map[string]any{}

	pol := opts.Subagents
	if pol.Enabled && pol.Model != "" {
		type agentDef struct {
			Model string `json:"model,omitempty"`
		}
		agents := map[string]agentDef{}
		for _, name := range agentNames {
			agents[name] = agentDef{Model: pol.Model}
		}
		cfg["agent"] = agents
	}

	if eco := opts.Economy; eco != nil && eco.On() {
		if eco.ToolOutput != nil {
			cfg["tool_output"] = map[string]any{
				"max_lines": eco.ToolOutput.MaxLines,
				"max_bytes": eco.ToolOutput.MaxBytes,
			}
		}
		if eco.Compaction != nil {
			cfg["compaction"] = map[string]any{
				"auto":   eco.Compaction.Auto,
				"keep":   map[string]any{"tokens": eco.Compaction.KeepTokens},
				"buffer": eco.Compaction.Buffer,
			}
		}
		if eco.Media != nil {
			cfg["media"] = map[string]any{
				"image": map[string]any{
					"auto_resize":      eco.Media.ImageResize,
					"max_width":        eco.Media.MaxWidth,
					"max_height":       eco.Media.MaxHeight,
					"max_base64_bytes": 1048576,
				},
			}
		}
	}

	if len(opts.Providers) > 0 {
		providers := map[string]any{}
		for id, o := range opts.Providers {
			entry := map[string]any{}
			if o.Name != "" {
				entry["name"] = o.Name
			}
			if o.Package != "" {
				entry["package"] = o.Package
			}
			if len(o.Env) > 0 {
				entry["env"] = o.Env
			}
			if o.BaseURL != "" {
				entry["settings"] = map[string]any{"baseURL": o.BaseURL}
			}
			if len(o.Headers) > 0 {
				entry["headers"] = o.Headers
			}
			if len(o.Models) > 0 {
				models := map[string]any{}
				for _, m := range o.Models {
					models[m] = map[string]any{"name": m}
				}
				entry["models"] = models
			}
			providers[id] = entry
		}
		cfg["providers"] = providers
	}

	if len(cfg) == 0 {
		return nil, noop, nil
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, noop, err
	}
	dir, err := os.MkdirTemp("", "warden-cfg-*")
	if err != nil {
		return nil, noop, err
	}
	path := filepath.Join(dir, "engine.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		os.RemoveAll(dir)
		return nil, noop, err
	}
	return []string{"OPENCODE_CONFIG=" + path}, func() { os.RemoveAll(dir) }, nil
}

// PrepareSubagents is a compatibility wrapper around PrepareConfig.
func PrepareSubagents(pol SubagentPolicy) ([]string, func(), error) {
	return PrepareConfig(ConfigOptions{Subagents: pol})
}

// Run executes one run against the engine, streaming events to onEvent.
func Run(ctx context.Context, opts Options, onEvent func(Event)) (*Result, ErrClass) {
	args := buildArgs(opts)
	cmd := exec.CommandContext(ctx, opts.Binary, args...)
	cmd.Dir = opts.Workdir
	cmd.Env = envFor(opts)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var mu sync.Mutex
	var stderr strings.Builder
	var streamed strings.Builder
	var streamErr strings.Builder

	cmd.Stderr = &redirectWriter{mu: &mu, sink: &stderr}
	var sessionMu sync.Mutex
	sessionID := opts.SessionID

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, ErrUnavailable
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return &Result{ExitCode: -1, Stderr: Sanitize(err.Error()), Duration: 0}, ErrUnavailable
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			mu.Lock()
			streamed.Write(line)
			streamed.WriteByte('\n')
			mu.Unlock()
			var ev Event
			if json.Unmarshal(line, &ev) != nil {
				continue
			}
			if ev.Error != nil && ev.Error.Message != "" {
				mu.Lock()
				streamErr.WriteString(ev.Error.Type + ": " + ev.Error.Message + "\n")
				mu.Unlock()
			}
			if ev.SessionID != "" {
				sessionMu.Lock()
				sessionID = ev.SessionID
				sessionMu.Unlock()
			}
			if onEvent != nil {
				onEvent(ev)
			}
		}
	}()

	timedOut := false
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	var exit int
	var werr error
	select {
	case werr = <-waitErr:
	case <-ctxDone(ctx):
		timedOut = true
		killGroup(cmd)
		werr = <-waitErr
	}
	<-done

	duration := time.Since(start)
	if werr != nil {
		if ee, ok := werr.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			exit = -1
		}
	}
	mu.Lock()
	stderrText := stderr.String()
	if stderrText == "" {
		stderrText = streamErr.String()
	}
	mu.Unlock()
	sessionMu.Lock()
	sid := sessionID
	sessionMu.Unlock()

	res := &Result{
		ExitCode:  exit,
		Stderr:    stderrText,
		Stdout:    streamed.String(),
		SessionID: sid,
		Duration:  duration,
	}
	cls := ErrNone
	if timedOut {
		cls = ErrTimeout
	} else if exit != 0 {
		cls = Classify(stderrText, exit, false)
	}
	return res, cls
}

func ctxDone(ctx context.Context) <-chan struct{} { return ctx.Done() }

func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	time.Sleep(500 * time.Millisecond)
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func buildArgs(opts Options) []string {
	args := []string{"run", "--format", "json"}
	if opts.Standalone {
		args = append(args, "--standalone")
	}
	if opts.AutoApprove {
		args = append(args, "--auto")
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	args = append(args, "--title", "warden-run")
	args = append(args, opts.Workdir, opts.Prompt)
	return args
}

func envFor(opts Options) []string {
	env := os.Environ()
	env = append(env, opts.ExtraEnv...)
	// silence engine logs so stderr carries only actionable errors
	env = append(env, "OPENCODE_LOG_LEVEL=error")
	return env
}

// The redirectWriter funnels stderr into a buffered sink.
type redirectWriter struct {
	mu   *sync.Mutex
	sink *strings.Builder
}

func (w *redirectWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sink.Write(p)
}
