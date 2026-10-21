package warden

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/esdras-schonevald/open-warden/internal/report"
)

// AgentNames lists configured agents; falls back to the requested/default one.
func (r *Runner) AgentNames(fallback string) []string {
	var names []string
	for name := range r.Cfg.Prompt.Agents {
		names = append(names, name)
	}
	if len(names) == 0 {
		if fallback != "" {
			return []string{fallback}
		}
		return []string{"default"}
	}
	sort.Strings(names)
	return names
}

// RunAgents runs every configured agent in parallel inside this process,
// sharing the proactive rate-limit ledger so the combined traffic never
// exceeds the configured windows.
func (r *Runner) RunAgents(ctx context.Context, task string, opts Options) ([]*report.Run, error) {
	names := r.AgentNames(opts.Agent)
	max := r.Cfg.MaxParallel
	if max <= 0 || max > len(names) {
		max = len(names)
	}
	sem := make(chan struct{}, max)
	var wg sync.WaitGroup
	runs := make([]*report.Run, len(names))
	errs := make([]error, len(names))
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			o := opts
			o.Agent = name
			// buffer per-agent text and print it in order after the run
			o.Quiet = true
			rep, err := r.Run(ctx, o)
			runs[i] = rep
			errs[i] = err
		}(i, name)
	}
	wg.Wait()

	var parts []string
	for i, rep := range runs {
		if errs[i] != nil && rep == nil {
			parts = append(parts, fmt.Sprintf("%s: %v", names[i], errs[i]))
		}
	}
	if len(parts) > 0 {
		return runs, fmt.Errorf("agents failed: %s", strings.Join(parts, "; "))
	}
	return runs, nil
}
