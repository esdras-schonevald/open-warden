// Package registry is the built-in catalog of providers and models exposed by
// open-warden. It is compiled into the binary: models, prices and rate limits
// are application decisions, not user configuration.
package registry

import (
	"os"
	"sort"
	"strings"

	"github.com/esdras-schonevald/open-warden/internal/config"
)

// Protocol identifies how the gateway talks to a provider.
type Protocol string

const (
	OpenAI    Protocol = "openai"
	Anthropic Protocol = "anthropic"
	Free      Protocol = "free"
)

// Provider is a supported upstream.
type Provider struct {
	ID       string
	Name     string
	Upstream string
	Env      []string
	KeyURL   string
	Protocol Protocol
}

// Model is a public model entry with an internal engine mapping.
type Model struct {
	Alias          string
	Label          string
	Provider       string // public provider id (free-tier for managed models)
	EngineProvider string // engine provider id (internal)
	EngineModel    string // upstream model id (internal)
	Context        int
	Price          config.Pricing
	Limits         config.Limits
}

// Ref is the internal engine model reference.
func (m Model) Ref() string { return m.EngineProvider + "/" + m.EngineModel }

// PublicRef is the provider/model reference shown to consumers.
func (m Model) PublicRef() string { return m.Provider + "/" + m.EngineModel }

var providers = map[string]Provider{
	"free-tier": {
		ID:       "free-tier",
		Name:     "Free tier",
		Protocol: Free,
	},
	"siliconflow": {
		ID:       "siliconflow",
		Name:     "SiliconFlow",
		Upstream: "https://api.siliconflow.com/v1",
		Env:      []string{"SILICONFLOW_API_KEY"},
		KeyURL:   "https://cloud.siliconflow.com/account/ak",
		Protocol: OpenAI,
	},
	"deepseek": {
		ID:       "deepseek",
		Name:     "DeepSeek",
		Upstream: "https://api.deepseek.com",
		Env:      []string{"DEEPSEEK_API_KEY"},
		KeyURL:   "https://platform.deepseek.com/api_keys",
		Protocol: OpenAI,
	},
	"openrouter": {
		ID:       "openrouter",
		Name:     "OpenRouter",
		Upstream: "https://openrouter.ai/api/v1",
		Env:      []string{"OPENROUTER_API_KEY"},
		KeyURL:   "https://openrouter.ai/settings/keys",
		Protocol: OpenAI,
	},
	"anthropic": {
		ID:       "anthropic",
		Name:     "Anthropic",
		Upstream: "https://api.anthropic.com",
		Env:      []string{"ANTHROPIC_API_KEY"},
		KeyURL:   "https://console.anthropic.com/settings/keys",
		Protocol: Anthropic,
	},
}

var models = []Model{
	// Managed free tier. Internally these are served by the engine's own
	// provider; publicly they are always "free-tier".
	{Alias: "free-glm", Label: "GLM-5.3 Flash (free)", Provider: "free-tier",
		EngineProvider: "opencode", EngineModel: "glm-5.3-flash-free", Context: 200000,
		Limits: config.Limits{RPM: 20, TPM: 200000}},
	{Alias: "free-deepseek", Label: "DeepSeek V4.1 Flash (free)", Provider: "free-tier",
		EngineProvider: "opencode", EngineModel: "deepseek-v4.1-flash-free", Context: 262144,
		Limits: config.Limits{RPM: 20, TPM: 200000}},
	{Alias: "free-minimax", Label: "MiniMax M3.1 Flash (free)", Provider: "free-tier",
		EngineProvider: "opencode", EngineModel: "minimax-m3.1-flash-free", Context: 200000,
		Limits: config.Limits{RPM: 20, TPM: 200000}},
	{Alias: "free-longcat", Label: "LongCat 2.5 Preview (free)", Provider: "free-tier",
		EngineProvider: "opencode", EngineModel: "longcat-2.5-preview-free", Context: 200000,
		Limits: config.Limits{RPM: 20, TPM: 200000}},

	// SiliconFlow
	{Alias: "qwen-coder-30b", Label: "Qwen3 Coder 30B", Provider: "siliconflow",
		EngineProvider: "siliconflow", EngineModel: "Qwen/Qwen3-Coder-30B-A3B-Instruct",
		Context: 262144, Price: config.Pricing{Input: 0.07, Output: 0.28},
		Limits: config.Limits{RPM: 300, TPM: 250000}},
	{Alias: "qwen3-30b", Label: "Qwen3 30B A3B", Provider: "siliconflow",
		EngineProvider: "siliconflow", EngineModel: "Qwen/Qwen3-30B-A3B-Instruct-2507",
		Context: 262000, Price: config.Pricing{Input: 0.09, Output: 0.30},
		Limits: config.Limits{RPM: 300, TPM: 250000}},
	{Alias: "glm-5.3-flash", Label: "GLM-5.3 Flash", Provider: "siliconflow",
		EngineProvider: "siliconflow", EngineModel: "zai-org/GLM-5.3-Flash",
		Context: 200000, Price: config.Pricing{Input: 0.15, Output: 0.50},
		Limits: config.Limits{RPM: 600, TPM: 500000}},

	// DeepSeek
	{Alias: "deepseek-flash", Label: "DeepSeek Flash", Provider: "deepseek",
		EngineProvider: "deepseek", EngineModel: "deepseek-flash",
		Context: 1048576, Price: config.Pricing{Input: 0.15, Output: 0.60, Reasoning: 0.60, CacheRead: 0.003}},
	{Alias: "deepseek-pro", Label: "DeepSeek V4 Pro", Provider: "deepseek",
		EngineProvider: "deepseek", EngineModel: "deepseek-v4-pro",
		Context: 1048576, Price: config.Pricing{Input: 0.66, Output: 1.98, Reasoning: 1.98, CacheRead: 0.022}},

	// OpenRouter
	{Alias: "or-qwen-coder-30b", Label: "Qwen3 Coder 30B (OpenRouter)", Provider: "openrouter",
		EngineProvider: "openrouter", EngineModel: "qwen/qwen3-coder-30b-a3b-instruct",
		Context: 262144, Price: config.Pricing{Input: 0.07, Output: 0.28}},

	// Anthropic
	{Alias: "claude-sonnet", Label: "Claude Sonnet 4.6", Provider: "anthropic",
		EngineProvider: "anthropic", EngineModel: "claude-sonnet-4-6",
		Context: 1000000, Price: config.Pricing{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}},
	{Alias: "claude-haiku", Label: "Claude Haiku 5.5", Provider: "anthropic",
		EngineProvider: "anthropic", EngineModel: "claude-haiku-5-5",
		Context: 1000000, Price: config.Pricing{Input: 0.10, Output: 0.50, CacheRead: 0.01, CacheWrite: 0.125}},
	{Alias: "claude-opus", Label: "Claude Opus 5", Provider: "anthropic",
		EngineProvider: "anthropic", EngineModel: "claude-opus-5",
		Context: 1000000, Price: config.Pricing{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25}},
}

// Providers returns all providers sorted by id.
func Providers() []Provider {
	out := make([]Provider, 0, len(providers))
	for _, p := range providers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// GetProvider returns a provider by id.
func GetProvider(id string) (Provider, bool) {
	p, ok := providers[id]
	return p, ok
}

// Models returns every model entry.
func Models() []Model {
	out := make([]Model, len(models))
	copy(out, models)
	return out
}

// Resolve maps an alias, a public ref or an internal ref to a model entry.
func Resolve(name string) (Model, bool) {
	name = strings.TrimSpace(name)
	for _, m := range models {
		if name == m.Alias || name == m.Ref() || name == m.PublicRef() ||
			name == m.EngineModel {
			return m, true
		}
	}
	return Model{}, false
}

// Available reports whether a model can run with the current environment.
func Available(m Model) bool {
	if m.Provider == "free-tier" {
		return true
	}
	p, ok := providers[m.Provider]
	if !ok {
		return false
	}
	for _, env := range p.Env {
		if os.Getenv(env) != "" {
			return true
		}
	}
	return false
}

// AvailableModels returns only models runnable with the current environment.
func AvailableModels() []Model {
	var out []Model
	for _, m := range models {
		if Available(m) {
			out = append(out, m)
		}
	}
	return out
}

// ProviderForRef resolves the provider that owns an internal model reference.
func ProviderForRef(ref string) (Provider, bool) {
	id := ref
	if i := strings.Index(id, "/"); i > 0 {
		id = id[:i]
	}
	if id == "opencode" {
		id = "free-tier"
	}
	return GetProvider(id)
}

// ModelForRef returns the registry entry matching an internal reference.
// Variant suffixes (#low/#high/...) are ignored for lookup.
func ModelForRef(ref string) (Model, bool) {
	base := ref
	if i := strings.Index(base, "#"); i >= 0 {
		base = base[:i]
	}
	for _, m := range models {
		if m.Ref() == base {
			return m, true
		}
	}
	return Model{}, false
}

// PublicName maps an internal model reference to its public alias. Unknown
// references are returned unchanged.
func PublicName(ref string) string {
	if m, ok := ModelForRef(ref); ok {
		return m.Alias
	}
	return ref
}

// ContextFor returns the context window for an internal reference.
func ContextFor(ref string) int {
	if m, ok := ModelForRef(ref); ok {
		return m.Context
	}
	return 0
}

// PriceFor returns the price for an internal reference.
func PriceFor(ref string) config.Pricing {
	if m, ok := ModelForRef(ref); ok {
		return m.Price
	}
	return config.Pricing{}
}

// LimitsFor returns the rate limits for an internal reference or public alias.
func LimitsFor(ref string) config.Limits {
	if m, ok := Resolve(ref); ok {
		return m.Limits
	}
	if m, ok := ModelForRef(ref); ok {
		return m.Limits
	}
	return config.Limits{}
}

// LimiterKey maps an internal reference to the public alias used in the
// persisted rate-limit windows. Unknown references (custom providers) are
// returned unchanged.
func LimiterKey(ref string) string {
	if m, ok := ModelForRef(ref); ok {
		return m.Alias
	}
	return ref
}

// GatewayBase returns the base URL the engine must use for a provider behind
// the local gateway.
func GatewayBase(gatewayURL string, p Provider) string {
	switch p.Protocol {
	case Anthropic:
		return gatewayURL + "/" + p.ID
	default:
		return gatewayURL + "/" + p.ID + "/v1"
	}
}
