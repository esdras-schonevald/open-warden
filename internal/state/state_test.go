package state

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestUpdateMergesConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := Update(path, func(f *File) {
				f.History = append(f.History, RunRecord{ID: fmt.Sprintf("r%d", i)})
			}); err != nil {
				t.Errorf("update: %v", err)
			}
		}(i)
	}
	wg.Wait()
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.History) != 20 {
		t.Fatalf("lost updates: %d", len(f.History))
	}
}

func TestPruneKeepsRecent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Now()
	if err := Update(path, func(f *File) {
		f.Windows["m"] = &ModelWindow{
			Requests: []time.Time{now.Add(-48 * time.Hour), now},
			Tokens:   []TokenEvent{{At: now.Add(-48 * time.Hour), Tokens: 1}, {At: now, Tokens: 2}},
		}
	}); err != nil {
		t.Fatal(err)
	}
	f, _ := Load(path)
	if len(f.Windows["m"].Requests) != 2 {
		t.Fatal("state should preserve raw windows; limiter prunes them")
	}
}
