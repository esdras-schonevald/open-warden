package registry

import (
	"os"
	"testing"
)

func TestRegistryIntegrity(t *testing.T) {
	seenAlias := map[string]bool{}
	seenRef := map[string]bool{}
	for _, m := range Models() {
		if m.Alias == "" || m.Label == "" || m.EngineModel == "" {
			t.Fatalf("incomplete model: %+v", m)
		}
		if seenAlias[m.Alias] {
			t.Fatalf("duplicate alias %q", m.Alias)
		}
		if seenRef[m.Ref()] {
			t.Fatalf("duplicate ref %q", m.Ref())
		}
		seenAlias[m.Alias] = true
		seenRef[m.Ref()] = true
		if _, ok := GetProvider(m.Provider); !ok {
			t.Fatalf("model %s references unknown provider %s", m.Alias, m.Provider)
		}
	}
}

func TestResolveAliasesAndRefs(t *testing.T) {
	m, ok := Resolve("qwen-coder-30b")
	if !ok || m.Ref() != "siliconflow/Qwen/Qwen3-Coder-30B-A3B-Instruct" {
		t.Fatalf("alias resolve: %+v %v", m, ok)
	}
	if got, ok := Resolve(m.Ref()); !ok || got.Alias != "qwen-coder-30b" {
		t.Fatalf("ref resolve: %+v %v", got, ok)
	}
	if _, ok := Resolve("nope"); ok {
		t.Fatal("unknown alias should not resolve")
	}
}

func TestFreeTierMapping(t *testing.T) {
	m, ok := Resolve("free-glm")
	if !ok {
		t.Fatal("free-glm missing")
	}
	if m.Provider != "free-tier" || m.EngineProvider != "opencode" {
		t.Fatalf("free mapping wrong: %+v", m)
	}
	p, ok := ProviderForRef(m.Ref())
	if !ok || p.Protocol != Free {
		t.Fatalf("free provider should be direct: %+v", p)
	}
}

func TestAvailability(t *testing.T) {
	old, had := os.LookupEnv("SILICONFLOW_API_KEY")
	os.Unsetenv("SILICONFLOW_API_KEY")
	defer func() {
		if had {
			os.Setenv("SILICONFLOW_API_KEY", old)
		}
	}()
	for _, m := range AvailableModels() {
		if m.Provider == "siliconflow" {
			t.Fatalf("siliconflow model should be unavailable without key: %s", m.Alias)
		}
	}
	os.Setenv("SILICONFLOW_API_KEY", "x")
	found := false
	for _, m := range AvailableModels() {
		if m.Provider == "siliconflow" {
			found = true
		}
	}
	if !found {
		t.Fatal("siliconflow models should be available with key")
	}
	for _, m := range AvailableModels() {
		if m.Provider == "free-tier" {
			return
		}
	}
	t.Fatal("free tier should always be available")
}

func TestGatewayBaseByProtocol(t *testing.T) {
	sf, _ := GetProvider("siliconflow")
	if got := GatewayBase("http://x", sf); got != "http://x/siliconflow/v1" {
		t.Fatalf("openai base: %s", got)
	}
	an, _ := GetProvider("anthropic")
	if got := GatewayBase("http://x", an); got != "http://x/anthropic" {
		t.Fatalf("anthropic base: %s", got)
	}
}

func TestPriceAndLimits(t *testing.T) {
	ref := "siliconflow/Qwen/Qwen3-Coder-30B-A3B-Instruct"
	if p := PriceFor(ref); p.Input != 0.07 || p.Output != 0.28 {
		t.Fatalf("price: %+v", p)
	}
	if l := LimitsFor(ref); l.RPM != 300 || l.TPM != 250000 {
		t.Fatalf("limits: %+v", l)
	}
}
