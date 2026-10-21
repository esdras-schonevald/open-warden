// Package config loads and validates warden configuration.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Pricing expresses USD per 1M tokens.
type Pricing struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	Reasoning  float64 `yaml:"reasoning"`
	CacheRead  float64 `yaml:"cache_read"`
	CacheWrite float64 `yaml:"cache_write"`
}

// Limits holds proactive rate limits applied per model.
type Limits struct {
	RPM    int                `yaml:"rpm"`
	TPM    int                `yaml:"tpm"`
	RPD    int                `yaml:"rpd"`
	TPD    int                `yaml:"tpd"`
	Models map[string]*Limits `yaml:"models"`
}

// For returns the effective limits for a model (model-specific overrides global).
func (l *Limits) For(model string) Limits {
	out := Limits{RPM: l.RPM, TPM: l.TPM, RPD: l.RPD, TPD: l.TPD}
	key := model
	if i := strings.Index(key, "#"); i >= 0 {
		key = key[:i]
	}
	m, ok := l.Models[key]
	if !ok {
		m, ok = l.Models[model]
	}
	if ok && m != nil {
		if m.RPM > 0 {
			out.RPM = m.RPM
		}
		if m.TPM > 0 {
			out.TPM = m.TPM
		}
		if m.RPD > 0 {
			out.RPD = m.RPD
		}
		if m.TPD > 0 {
			out.TPD = m.TPD
		}
	}
	return out
}

// ModelPrice resolves pricing for a model, falling back to the default table.
// Keys may include a variant suffix (e.g. "provider/model#high") which is
// ignored when matching the base model.
func (c *Config) ModelPrice(model string) Pricing {
	base := model
	if i := strings.Index(base, "#"); i >= 0 {
		base = base[:i]
	}
	if p, ok := c.Pricing[model]; ok {
		return p
	}
	if p, ok := c.Pricing[base]; ok {
		return p
	}
	return c.Pricing["default"]
}

// AgentSpec describes one named agent.
type AgentSpec struct {
	Name       string `yaml:"name"`
	Workdir    string `yaml:"workdir"`
	Model      string `yaml:"model"`
	Fallback   string `yaml:"fallback"`
	PromptFile string `yaml:"prompt_file"`
	Prompt     string `yaml:"prompt"`
}

// FallbackTrigger names the failure classes that activate the fallback chain.
type FallbackTrigger string

const (
	TriggerRateLimit   FallbackTrigger = "rate_limit"
	TriggerQuota       FallbackTrigger = "quota"
	TriggerUnavailable FallbackTrigger = "unavailable"
)

// Provider describes an upstream provider that warden can route through its
// local request gateway.
type Provider struct {
	Upstream string   `yaml:"upstream"`
	Env      []string `yaml:"env"`
}

// GatewayOptions controls the local request gateway.
type GatewayOptions struct {
	Enabled *bool         `yaml:"enabled"`
	MaxWait time.Duration `yaml:"max_wait"`
}

// CatalogEntry is a model exposed to consumers under a stable alias. The
// underlying engine model id stays internal.
type CatalogEntry struct {
	Alias   string   `yaml:"alias"`
	Label   string   `yaml:"label"`
	Model   string   `yaml:"model"`
	Context int      `yaml:"context"`
	Pricing *Pricing `yaml:"pricing"`
}

// Subagents defines the subagent policy surfaced to the engine.
type Subagents struct {
	Model           string `yaml:"model"`
	FallbackModel   string `yaml:"fallback_model"`
	Instruction     string `yaml:"instruction"`
	InstructionFile string `yaml:"instruction_file"`
}

// Prompt carries global and per-agent prompts.
type Prompt struct {
	Global     string               `yaml:"global"`
	GlobalFile string               `yaml:"global_file"`
	Agents     map[string]AgentSpec `yaml:"agents"`
}

// RunOptions tunes a single run.
type RunOptions struct {
	Timeout        time.Duration `yaml:"timeout"`
	PollInterval   time.Duration `yaml:"poll_interval"`
	AutoApprove    *bool         `yaml:"auto_approve"`
	EstDivisor     float64       `yaml:"estimate_divisor"`
	OutputHeadroom int           `yaml:"output_headroom"`
}

// Auto reports whether tool permissions are auto-approved (default true).
func (r RunOptions) Auto() bool { return r.AutoApprove == nil || *r.AutoApprove }

// Config is the top-level warden configuration.
type Config struct {
	EngineBin   string              `yaml:"engine_bin"`
	Standalone  bool                `yaml:"standalone"`
	Model       string              `yaml:"model"`
	Fallbacks   []string            `yaml:"fallbacks"`
	Limits      *Limits             `yaml:"limits"`
	Pricing     map[string]Pricing  `yaml:"pricing"`
	Budget      float64             `yaml:"budget"`
	MaxParallel int                 `yaml:"max_parallel"`
	Fallback    FallbackPolicy      `yaml:"fallback"`
	Prompt      Prompt              `yaml:"prompt"`
	Subagents   Subagents           `yaml:"subagents"`
	Run         RunOptions          `yaml:"run"`
	StateFile   string              `yaml:"state_file"`
	Instruction string              `yaml:"-"`
	Providers   map[string]Provider `yaml:"providers"`
	Gateway     GatewayOptions      `yaml:"gateway"`
	Catalog     []CatalogEntry      `yaml:"catalog"`
}

// FallbackPolicy lists triggers handled by the fallback chain.
type FallbackPolicy struct {
	On          []FallbackTrigger `yaml:"on"`
	Subagents   bool              `yaml:"subagents"`
	WaitBlocked bool              `yaml:"wait_when_all_blocked"`
	MaxRetries  int               `yaml:"max_retries"`
}

// Defaults fills unset fields with sane values.
func (c *Config) Defaults() {
	if c.EngineBin == "" {
		c.EngineBin = "opencode"
	}
	if c.Model == "" && len(c.Fallbacks) > 0 {
		c.Model = c.Fallbacks[0]
	}
	if c.Limits == nil {
		c.Limits = &Limits{}
	}
	if c.Pricing == nil {
		c.Pricing = map[string]Pricing{}
	}
	if len(c.Fallback.On) == 0 {
		c.Fallback.On = []FallbackTrigger{
			TriggerRateLimit, TriggerQuota, TriggerUnavailable,
		}
	}
	if c.Fallback.MaxRetries == 0 {
		c.Fallback.MaxRetries = len(c.Fallbacks) + 1
		c.Fallback.MaxRetries *= 2
	}
	if c.Run.Timeout == 0 {
		c.Run.Timeout = 30 * time.Minute
	}
	if c.Run.PollInterval == 0 {
		c.Run.PollInterval = 2 * time.Second
	}
	if c.Run.EstDivisor == 0 {
		c.Run.EstDivisor = 4
	}
	if c.Run.OutputHeadroom == 0 {
		c.Run.OutputHeadroom = 4096
	}
	if c.StateFile == "" {
		c.StateFile = defaultStatePath()
	}
	if c.Gateway.MaxWait == 0 {
		c.Gateway.MaxWait = 20 * time.Second
	}
}

func defaultStatePath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = home + "/.local/state"
	}
	return base + "/open-warden/state.json"
}

// Load reads a YAML config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	c := &Config{}
	if err := yaml.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.Defaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate checks mandatory fields.
func (c *Config) Validate() error {
	if c.Model == "" {
		return fmt.Errorf("config: model is required")
	}
	return nil
}

// GlobalPrompt returns the global prompt text, loading from file when needed.
func (c *Config) GlobalPrompt() (string, error) {
	if c.Prompt.GlobalFile != "" {
		raw, err := os.ReadFile(c.Prompt.GlobalFile)
		if err != nil {
			return "", fmt.Errorf("read global prompt: %w", err)
		}
		return strings.TrimRight(string(raw), "\n"), nil
	}
	return c.Prompt.Global, nil
}

// Agent returns the agent spec by name.
func (c *Config) Agent(name string) (AgentSpec, bool) {
	a, ok := c.Prompt.Agents[name]
	return a, ok
}

// GatewayEnabled reports whether the request gateway should run.
func (c *Config) GatewayEnabled() bool {
	if c.Gateway.Enabled != nil {
		return *c.Gateway.Enabled
	}
	return len(c.Providers) > 0
}

// ResolveModel maps a public alias to the internal model id.
func (c *Config) ResolveModel(name string) string {
	for _, e := range c.Catalog {
		if e.Alias == name && e.Model != "" {
			return e.Model
		}
	}
	return name
}

// CatalogFor returns the catalog entry matching an internal model id.
func (c *Config) CatalogFor(model string) (CatalogEntry, bool) {
	for _, e := range c.Catalog {
		if e.Model == model {
			return e, true
		}
	}
	return CatalogEntry{}, false
}

// EffectivePrice returns the catalog price when set, else the global table.
func (c *Config) EffectivePrice(model string) Pricing {
	if e, ok := c.CatalogFor(model); ok && e.Pricing != nil {
		return *e.Pricing
	}
	return c.ModelPrice(model)
}

// ProviderOf extracts the provider id from a provider/model reference.
func ProviderOf(model string) string {
	if i := strings.Index(model, "/"); i > 0 {
		return model[:i]
	}
	return ""
}

// ProviderFor returns the gateway provider config for a model.
func (c *Config) ProviderFor(model string) (Provider, bool) {
	p, ok := c.Providers[ProviderOf(model)]
	return p, ok
}
