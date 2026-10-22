// Package policy holds fixed, application-defined instructions injected into
// every run. Users cannot override or disable them.
package policy

import (
	_ "embed"
	"strings"
)

//go:embed delegation.md
var delegation string

//go:embed economy_balanced.md
var economyBalanced string

//go:embed economy_aggressive.md
var economyAggressive string

// Delegation is the fixed subagent policy. It is always applied, regardless of
// the economy mode.
func Delegation() string { return strings.TrimSpace(delegation) }

// Economy returns the fixed economy instructions for a mode, or "" when the
// mode is off.
func Economy(mode string) string {
	switch mode {
	case "balanced":
		return strings.TrimSpace(economyBalanced)
	case "aggressive":
		return strings.TrimSpace(economyAggressive)
	default:
		return ""
	}
}
