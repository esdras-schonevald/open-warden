// Package state persists rate-limit accounting and run history across invocations.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TokenEvent records one request admission with its token weight.
type TokenEvent struct {
	At     time.Time `json:"at"`
	Tokens int       `json:"tokens"`
	Model  string    `json:"model"`
}

// ModelWindow holds the sliding-window counters for one model.
type ModelWindow struct {
	Requests []time.Time  `json:"requests"`
	Tokens   []TokenEvent `json:"tokens"`
}

// File is the persisted state document.
type File struct {
	History []RunRecord             `json:"history"`
	Windows map[string]*ModelWindow `json:"windows"`
}

// RunRecord summarizes one finished run for reporting.
type RunRecord struct {
	ID             string    `json:"id"`
	Agent          string    `json:"agent"`
	Model          string    `json:"model"`
	Started        time.Time `json:"started"`
	DurationSec    float64   `json:"duration_sec"`
	Input          int       `json:"tokens_input"`
	Output         int       `json:"tokens_output"`
	Reasoning      int       `json:"tokens_reasoning"`
	CacheRead      int       `json:"tokens_cache_read"`
	CacheWrite     int       `json:"tokens_cache_write"`
	SubInput       int       `json:"subagent_tokens_input"`
	SubOutput      int       `json:"subagent_tokens_output"`
	SubReasoning   int       `json:"subagent_tokens_reasoning"`
	SubCacheRead   int       `json:"subagent_tokens_cache_read"`
	SubCacheWrite  int       `json:"subagent_tokens_cache_write"`
	Requests       int       `json:"requests"`
	Cost           float64   `json:"cost"`
	Budget         float64   `json:"budget"`
	BudgetExceeded bool      `json:"budget_exceeded"`
	Fallbacks      []string  `json:"fallback_events"`
	RateWaits      int       `json:"rate_limit_waits"`
	Status         string    `json:"status"`
}

var mu sync.Mutex

// Load reads the state file (creating it if absent).
func Load(path string) (*File, error) {
	mu.Lock()
	defer mu.Unlock()
	f := &File{Windows: map[string]*ModelWindow{}}
	raw, err := os.ReadFile(path)
	if err == nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, f); err != nil {
			return nil, err
		}
		if f.Windows == nil {
			f.Windows = map[string]*ModelWindow{}
		}
	} else if !os.IsNotExist(err) && err != nil {
		return nil, err
	}
	return f, nil
}

// Save atomically persists the state.
func Save(path string, f *File) error {
	mu.Lock()
	defer mu.Unlock()
	return writeLocked(path, f)
}

// Update merges a run's data into the on-disk state under a process-wide lock,
// so parallel agents never lose each other's history or windows.
func Update(path string, fn func(*File)) error {
	mu.Lock()
	defer mu.Unlock()
	f := &File{Windows: map[string]*ModelWindow{}}
	if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, f); err != nil {
			return err
		}
		if f.Windows == nil {
			f.Windows = map[string]*ModelWindow{}
		}
	}
	fn(f)
	return writeLocked(path, f)
}

func writeLocked(path string, f *File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
