package report

import (
	"strings"
	"testing"
	"time"

	"github.com/esdras-schonevald/open-warden/internal/usage"
)

func TestTextReport(t *testing.T) {
	r := &Run{
		ID: "ses_1", Agent: "builder", Model: "p/m", Budget: 2, Cost: 0.5,
		Status: "ok", DurationSec: 3,
		Usage: &usage.Report{
			Root:      usage.Totals{Input: 100, Output: 20, Reasoning: 5, CacheRead: 30, Requests: 1, Cost: 0.4},
			Subagents: usage.Totals{Input: 10, Output: 2, Requests: 1, Cost: 0.1},
			Total:     usage.Totals{Input: 110, Output: 22, Reasoning: 5, CacheRead: 30, Requests: 2, Cost: 0.5},
			ByModel:   map[string]usage.Totals{"p/m": {Input: 110, Cost: 0.5}},
		},
		Fallbacks: []FallbackEvent{{From: "a", To: "b", Reason: "rate_limit", Scope: "main", At: time.Now()}},
	}
	out := r.Text()
	for _, want := range []string{"subagents", "p/m", "fallback events", "a → b", "budget $2.00"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

func TestJSONReport(t *testing.T) {
	r := &Run{ID: "ses_1", Usage: &usage.Report{ByModel: map[string]usage.Totals{}}}
	raw, err := r.JSON()
	if err != nil || !strings.Contains(raw, `"usage"`) {
		t.Fatalf("bad json: %v %s", err, raw)
	}
}
