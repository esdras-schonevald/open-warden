package engine

import (
	"os"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		stderr string
		exit   int
		want   ErrClass
	}{
		{"Error: 429 Too Many Requests", 1, ErrRateLimit},
		{"insufficient_balance", 1, ErrQuota},
		{"payment required", 1, ErrQuota},
		{"connection reset", 1, ErrUnavailable},
		{"", 0, ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.stderr, c.exit, false); got != c.want {
			t.Errorf("Classify(%q)=%s want %s", c.stderr, got, c.want)
		}
	}
	if got := Classify("", 0, true); got != ErrTimeout {
		t.Errorf("timeout not detected: %s", got)
	}
}

func TestSanitize(t *testing.T) {
	in := "opencode failed (OpenCode v2) OPENCODE_TOKEN"
	out := Sanitize(in)
	if strings.Contains(strings.ToLower(out), "opencode") {
		t.Fatalf("engine branding leaked: %s", out)
	}
}

func TestBuildArgs(t *testing.T) {
	args := buildArgs(Options{
		Model: "p/m", SessionID: "ses_1", Prompt: "hi",
		Workdir: "/w", AutoApprove: true, Standalone: true,
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{"run", "--format json", "--standalone", "--auto",
		"--model p/m", "/w hi"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}
}

func TestPrepareSubagents(t *testing.T) {
	env, cleanup, err := PrepareSubagents(SubagentPolicy{Enabled: true, Model: "p/sub", Instruction: "be brief"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(env) != 1 || !strings.HasPrefix(env[0], "OPENCODE_CONFIG=") {
		t.Fatalf("unexpected env: %v", env)
	}
	if _, err := os.Stat(strings.TrimPrefix(env[0], "OPENCODE_CONFIG=")); err != nil {
		t.Fatalf("config file missing: %v", err)
	}
}
