package limiter

import (
	"testing"
	"time"

	"github.com/esdras-schonevald/open-warden/internal/config"
)

func TestRPMBlocksProactively(t *testing.T) {
	l := New(&config.Limits{RPM: 2}, nil)
	l.Consume("m", 10)
	l.Consume("m", 10)
	if a := l.Admit("m", 10); a.Allowed {
		t.Fatalf("third request should be blocked by rpm=2")
	} else if a.WaitFor <= 0 || a.WaitFor > time.Minute {
		t.Fatalf("unexpected wait %s", a.WaitFor)
	}
}

func TestTPMWindow(t *testing.T) {
	l := New(&config.Limits{TPM: 1000}, nil)
	l.Consume("m", 900)
	if a := l.Admit("m", 200); a.Allowed {
		t.Fatalf("request should exceed tpm")
	}
	if a := l.Admit("m", 100); !a.Allowed {
		t.Fatalf("request within tpm should be allowed: %+v", a)
	}
}

func TestModelOverride(t *testing.T) {
	lim := &config.Limits{RPM: 100, Models: map[string]*config.Limits{
		"fast": {RPM: 1},
	}}
	l := New(lim, nil)
	l.Consume("fast", 1)
	if a := l.Admit("fast", 1); a.Allowed {
		t.Fatal("model override rpm=1 should block")
	}
	if a := l.Admit("other", 1); !a.Allowed {
		t.Fatal("other model should use global rpm=100")
	}
}

func TestAdjust(t *testing.T) {
	l := New(&config.Limits{TPM: 1000}, nil)
	at := l.Consume("m", 1000)
	if a := l.Admit("m", 1); a.Allowed {
		t.Fatal("expected block")
	}
	l.Adjust("m", at, 10)
	if a := l.Admit("m", 100); !a.Allowed {
		t.Fatal("after reconciliation tokens should be freed")
	}
}

func TestEarliestWaitChoosesClosest(t *testing.T) {
	l := New(&config.Limits{Models: map[string]*config.Limits{
		"a": {RPM: 1},
		"b": {RPM: 1},
	}}, nil)
	l.Consume("a", 1)
	l.Consume("b", 1)
	w := l.EarliestWait([]string{"a", "b"}, 1)
	if w <= 0 || w > time.Minute {
		t.Fatalf("unexpected wait %s", w)
	}
}
