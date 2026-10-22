// Package report renders final run reports (text and JSON).
package report

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/esdras-schonevald/open-warden/internal/engine"
	"github.com/esdras-schonevald/open-warden/internal/usage"
)

// FallbackEvent records one fallback decision.
type FallbackEvent struct {
	At      time.Time `json:"at"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	Reason  string    `json:"reason"`
	Scope   string    `json:"scope"` // main | subagents
	WaitSec float64   `json:"waited_seconds,omitempty"`
}

// Run ties together everything worth reporting about a run.
type Run struct {
	ID                string          `json:"id"`
	Agent             string          `json:"agent"`
	Started           time.Time       `json:"started"`
	DurationSec       float64         `json:"duration_sec"`
	Model             string          `json:"model"`
	EconomyMode       string          `json:"economy_mode,omitempty"`
	SubagentsModel    string          `json:"subagents_model,omitempty"`
	SubagentsFallback string          `json:"subagents_fallback,omitempty"`
	Status            string          `json:"status"`
	Budget            float64         `json:"budget"`
	Cost              float64         `json:"cost"`
	BudgetExceeded    bool            `json:"budget_exceeded"`
	Usage             *usage.Report   `json:"usage"`
	Fallbacks         []FallbackEvent `json:"fallback_events"`
	RateWaits         int             `json:"rate_limit_waits"`
	OutputCapApplied  int             `json:"output_cap_applied"`
	OutputCapTokens   int             `json:"output_cap_tokens,omitempty"`
	OutputTruncated   int             `json:"output_truncated"`
	Output            string          `json:"output"`
	ErrorDetail       string          `json:"error,omitempty"`
}

// JSON renders the report as machine-readable JSON.
func (r *Run) JSON() (string, error) {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func bar(v int, total int) string {
	if total <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d", v)
}

// Text renders a human-readable summary.
func (r *Run) Text() string {
	var b strings.Builder
	u := r.Usage
	w := func(label string, t usage.Totals) {
		fmt.Fprintf(&b, "  %-12s in=%s out=%s reasoning=%s cache_read=%s cache_write=%s requests=%d cost=$%.6f\n",
			label, bar(t.Input, 1), bar(t.Output, 1), bar(t.Reasoning, 1),
			bar(t.CacheRead, 1), bar(t.CacheWrite, 1), t.Requests, t.Cost)
	}
	fmt.Fprintf(&b, "\n=== warden report | %s | %s ===\n", r.Agent, r.ID)
	fmt.Fprintf(&b, "status: %s (budget $%.2f, spent $%.6f)", r.Status, r.Budget, r.Cost)
	if r.BudgetExceeded {
		fmt.Fprintf(&b, " — BUDGET REACHED, run stopped early")
	}
	b.WriteByte('\n')
	if u != nil {
		fmt.Fprintf(&b, "tokens:\n")
		w("main", u.Root)
		w("subagents", u.Subagents)
		w("total", u.Total)
		if len(u.ByModel) > 0 {
			fmt.Fprintf(&b, "by model:\n")
			for m, t := range u.ByModel {
				fmt.Fprintf(&b, "  %-40s in=%d out=%d reasoning=%d cache_read=%d cache_write=%d requests=%d cost=$%.6f\n",
					m, t.Input, t.Output, t.Reasoning, t.CacheRead, t.CacheWrite, t.Requests, t.Cost)
			}
		}
	}
	if len(r.Fallbacks) > 0 {
		fmt.Fprintf(&b, "fallback events:\n")
		for _, f := range r.Fallbacks {
			fmt.Fprintf(&b, "  [%s] %s → %s (%s) reason=%s wait=%.1fs\n",
				f.Scope, f.From, f.To, f.At.Format("15:04:05"), f.Reason, f.WaitSec)
		}
	}
	if r.RateWaits > 0 {
		fmt.Fprintf(&b, "rate-limit waits: %d\n", r.RateWaits)
	}
	fmt.Fprintf(&b, "duration: %.1fs\n", r.DurationSec)
	if r.ErrorDetail != "" {
		fmt.Fprintf(&b, "error: %s\n", engine.Sanitize(r.ErrorDetail))
	}
	return b.String()
}
