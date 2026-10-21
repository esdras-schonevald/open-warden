// Package limiter implements proactive TPM/RPM/TPD/RPD control.
//
// Admission is decided BEFORE the request is dispatched: if the next request
// would exceed any configured window, the caller can immediately route the
// work to a fallback model instead of waiting for a rate-limit error.
package limiter

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/esdras-schonevald/open-warden/internal/config"
	"github.com/esdras-schonevald/open-warden/internal/state"
)

// Limiter tracks sliding windows per model backed by persistent state.
type Limiter struct {
	mu      sync.Mutex
	limits  *config.Limits
	windows map[string]*state.ModelWindow
}

// New builds a limiter over persistent state windows.
func New(limits *config.Limits, windows map[string]*state.ModelWindow) *Limiter {
	if limits == nil {
		limits = &config.Limits{}
	}
	if windows == nil {
		windows = map[string]*state.ModelWindow{}
	}
	return &Limiter{limits: limits, windows: windows}
}

func normalize(model string) string {
	if i := strings.Index(model, "#"); i >= 0 {
		return model[:i]
	}
	return model
}

func (l *Limiter) window(model string) *state.ModelWindow {
	model = normalize(model)
	w, ok := l.windows[model]
	if !ok {
		w = &state.ModelWindow{}
		l.windows[model] = w
	}
	return w
}

// prune drops events older than 24h and returns live slices.
func prune(w *state.ModelWindow, now time.Time) (reqs []time.Time, toks []state.TokenEvent) {
	cutoff := now.Add(-24 * time.Hour)
	reqs = w.Requests[:0]
	for _, t := range w.Requests {
		if t.After(cutoff) {
			reqs = append(reqs, t)
		}
	}
	toks = w.Tokens[:0]
	for _, e := range w.Tokens {
		if e.At.After(cutoff) {
			toks = append(toks, e)
		}
	}
	return reqs, toks
}

func countSince(reqs []time.Time, toks []state.TokenEvent, now time.Time, d time.Duration) (int, int) {
	cutoff := now.Add(-d)
	req, tok := 0, 0
	for _, t := range reqs {
		if t.After(cutoff) {
			req++
		}
	}
	for _, e := range toks {
		if e.At.After(cutoff) {
			tok += e.Tokens
		}
	}
	return req, tok
}

// nextFree returns the earliest instant when every window for the model
// admits one more request with est tokens.
func nextFree(l config.Limits, w *state.ModelWindow, est int, now time.Time) time.Time {
	var next time.Time
	push := func(t time.Time) {
		if t.After(now) && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	reqs, toks := prune(w, now)
	if l.RPM > 0 && len(reqs) >= l.RPM {
		push(reqs[len(reqs)-l.RPM].Add(time.Minute))
	}
	if l.TPM > 0 {
		minute := make([]state.TokenEvent, 0, len(toks))
		for _, e := range toks {
			if e.At.After(now.Add(-time.Minute)) {
				minute = append(minute, e)
			}
		}
		if sum(minute)+est > l.TPM {
			push(minute[0].At.Add(time.Minute))
		}
	}
	if l.RPD > 0 && len(reqs) >= l.RPD {
		push(reqs[len(reqs)-l.RPD].Add(24 * time.Hour))
	}
	if l.TPD > 0 {
		if sum(toks)+est > l.TPD {
			// oldest relevant token event determines the free slot
			if len(toks) > 0 {
				push(toks[0].At.Add(24 * time.Hour))
			} else {
				push(now)
			}
		}
	}
	return next
}

func sum(evs []state.TokenEvent) int {
	n := 0
	for _, e := range evs {
		n += e.Tokens
	}
	return n
}

// Admission reports whether a dispatch is allowed now.
type Admission struct {
	Allowed  bool
	WaitFor  time.Duration // >0 when blocked; time until the window frees
	Requests int           // live per-minute/dia counts (diagnostic)
	Tokens   int
}

// Admit checks (without consuming) whether the model accepts one more
// request of estTokens right now.
func (l *Limiter) Admit(model string, estTokens int) Admission {
	l.mu.Lock()
	defer l.mu.Unlock()
	model = normalize(model)
	now := time.Now()
	lm := l.limits.For(model)
	w := l.window(model)
	reqs, toks := prune(w, now)
	req1, tok1 := countSince(reqs, toks, now, time.Minute)
	free := nextFree(lm, w, estTokens, now)
	var wait time.Duration
	if !free.IsZero() {
		wait = free.Sub(now)
	}
	return Admission{
		Allowed:  free.IsZero() || wait <= 0,
		WaitFor:  wait,
		Requests: req1,
		Tokens:   tok1,
	}
}

// EarliestWait returns when any model in the chain would admit the request,
// used to decide between waiting and failing fast.
func (l *Limiter) EarliestWait(models []string, estTokens int) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	var earliest time.Time
	for _, m := range models {
		lm := l.limits.For(m)
		w := l.window(m)
		free := nextFree(lm, w, estTokens, now)
		if !free.IsZero() && (earliest.IsZero() || free.Before(earliest)) {
			earliest = free
		}
	}
	if earliest.IsZero() {
		return 0
	}
	return earliest.Sub(now)
}

// Consume records one admitted request with its estimated token weight and
// returns the event timestamp used later to reconcile the estimate.
func (l *Limiter) Consume(model string, estTokens int) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	model = normalize(model)
	now := time.Now()
	w := l.window(model)
	reqs, toks := prune(w, now)
	reqs = append(reqs, now)
	toks = append(toks, state.TokenEvent{At: now, Tokens: estTokens, Model: model})
	w.Requests, w.Tokens = reqs, toks
	return now
}

// Adjust reconciles an estimated reservation with actual usage. The timestamp
// returned by Consume identifies the exact event, so parallel agents cannot
// corrupt each other's accounting.
func (l *Limiter) Adjust(model string, at time.Time, actualTokens int) {
	if actualTokens <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	model = normalize(model)
	now := time.Now()
	w := l.window(model)
	reqs, toks := prune(w, now)
	for i := range toks {
		if toks[i].Model == model && toks[i].At.Equal(at) {
			toks[i].Tokens = actualTokens
			break
		}
	}
	w.Requests, w.Tokens = reqs, toks
}

// WithLock runs fn while holding the limiter lock so callers can snapshot
// and persist windows atomically.
func (l *Limiter) WithLock(fn func(windows map[string]*state.ModelWindow)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fn(l.windows)
}

// Snapshot returns the current windows sorted for stable persistence.
func (l *Limiter) Snapshot() map[string]*state.ModelWindow {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]*state.ModelWindow, len(l.windows))
	for k, v := range l.windows {
		cp := *v
		out[k] = &cp
	}
	return out
}

// SortedModels lists tracked models deterministically.
func SortedModels(windows map[string]*state.ModelWindow) []string {
	out := make([]string, 0, len(windows))
	for k := range windows {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
