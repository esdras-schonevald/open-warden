package usage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func fixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "engine.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`
		CREATE TABLE session_v2 (
			id TEXT PRIMARY KEY, parent_id TEXT, model TEXT, cost REAL,
			tokens_input INTEGER, tokens_output INTEGER, tokens_reasoning INTEGER,
			tokens_cache_read INTEGER, tokens_cache_write INTEGER
		);
		INSERT INTO session_v2 VALUES
			('root', NULL, '{"id":"glm","providerID":"siliconflow"}', 0.10, 100, 20, 5, 30, 1),
			('child', 'root', '{"id":"sub","providerID":"siliconflow"}', 0.05, 50, 10, 2, 0, 0),
			('grand', 'child', '{"id":"sub","providerID":"siliconflow"}', 0.02, 25, 5, 1, 0, 0),
			('unrelated', NULL, '{"id":"x","providerID":"y"}', 9.99, 999, 999, 0, 0, 0);
		CREATE TABLE session_message (id TEXT, session_id TEXT, type TEXT);
		INSERT INTO session_message VALUES
			('m1','root','assistant'), ('m2','root','assistant'),
			('m3','child','assistant'), ('m4','root','user'),
			('m5','unrelated','assistant');
	`)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCollectSessionTree(t *testing.T) {
	t.Setenv("ENGINE_DB", fixture(t))
	rep, err := Collect("root", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Root.Input != 100 || rep.Root.Output != 20 {
		t.Fatalf("root totals wrong: %+v", rep.Root)
	}
	if rep.Subagents.Input != 75 || rep.Subagents.Output != 15 {
		t.Fatalf("subagent totals wrong: %+v", rep.Subagents)
	}
	if rep.Total.Input != 175 || rep.Total.Cost != 0.17 {
		t.Fatalf("total wrong: %+v", rep.Total)
	}
	if got := rep.ByModel["siliconflow/glm"].Input; got != 100 {
		t.Fatalf("by model key wrong: %v (%v)", got, rep.ByModel)
	}
	if len(rep.ByModel) != 2 {
		t.Fatalf("unexpected models: %v", rep.ByModel)
	}
	if rep.Root.Requests != 2 || rep.Subagents.Requests != 1 || rep.Total.Requests != 3 {
		t.Fatalf("request counts wrong: root=%d sub=%d total=%d",
			rep.Root.Requests, rep.Subagents.Requests, rep.Total.Requests)
	}
}

func TestCollectMissingDB(t *testing.T) {
	t.Setenv("ENGINE_DB", filepath.Join(t.TempDir(), "nope.db"))
	if _, err := Collect("root", time.Time{}); err == nil {
		t.Fatal("expected error for missing db")
	}
}

func TestModelKeyFallback(t *testing.T) {
	if got := modelKey("plain-model"); got != "plain-model" {
		t.Fatalf("got %s", got)
	}
}
