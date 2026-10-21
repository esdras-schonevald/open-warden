// Package usage aggregates token consumption and cost for a run, walking the
// full session tree (root + subagents) from the engine's local database.
package usage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Totals sums token usage for a set of sessions.
type Totals struct {
	Requests   int     `json:"requests"`
	Input      int     `json:"tokens_input"`
	Output     int     `json:"tokens_output"`
	Reasoning  int     `json:"tokens_reasoning"`
	CacheRead  int     `json:"tokens_cache_read"`
	CacheWrite int     `json:"tokens_cache_write"`
	Cost       float64 `json:"cost"`
}

// Add merges another totals object into this one.
func (t *Totals) Add(o Totals) {
	t.Requests += o.Requests
	t.Input += o.Input
	t.Output += o.Output
	t.Reasoning += o.Reasoning
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
	t.Cost += o.Cost
}

// Report is the complete usage picture of one run.
type Report struct {
	Root      Totals            `json:"root"`
	Subagents Totals            `json:"subagents"`
	Total     Totals            `json:"total"`
	ByModel   map[string]Totals `json:"by_model"`
}

// DBPath resolves the engine database location.
func DBPath() (string, error) {
	if p := os.Getenv("ENGINE_DB"); p != "" {
		return p, nil
	}
	if p := os.Getenv("OPENCODE_DB"); p != "" {
		return p, nil
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "opencode", "opencode.db"), nil
}

func open() (*sql.DB, string, error) {
	path, err := DBPath()
	if err != nil {
		return nil, "", err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, "", fmt.Errorf("engine database not found: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, "", err
	}
	return db, path, nil
}

// sessionsFor walks parent_id recursively from a root session.
func sessionsFor(db *sql.DB, root string) (map[string]bool, error) {
	out := map[string]bool{root: true}
	frontier := []string{root}
	for len(frontier) > 0 {
		var next []string
		for _, id := range frontier {
			rows, err := db.Query(
				`SELECT id FROM session_v2 WHERE parent_id = ?`, id)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var cid string
				if err := rows.Scan(&cid); err == nil && !out[cid] {
					out[cid] = true
					next = append(next, cid)
				}
			}
			rows.Close()
		}
		frontier = next
	}
	return out, nil
}

// Collect builds the usage report for a run's session tree. Discount marks
// sessions created before the run started (they are excluded).
func Collect(root string, since time.Time) (*Report, error) {
	if root == "" {
		return &Report{ByModel: map[string]Totals{}}, nil
	}
	db, _, err := open()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	ids, err := sessionsFor(db, root)
	if err != nil {
		return nil, err
	}
	rep := &Report{ByModel: map[string]Totals{}}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	query := fmt.Sprintf(`
		SELECT id, parent_id, model, cost,
		       tokens_input, tokens_output, tokens_reasoning,
		       tokens_cache_read, tokens_cache_write
		FROM session_v2
		WHERE id IN (%s)`, strings.Join(placeholders, ","))

	// Real request counts come from assistant messages, not session rows.
	counts := map[string]int{}
	if crows, cerr := db.Query(fmt.Sprintf(`
		SELECT session_id, COUNT(*) FROM session_message
		WHERE type = 'assistant' AND session_id IN (%s)
		GROUP BY session_id`, strings.Join(placeholders, ",")), args...); cerr == nil {
		for crows.Next() {
			var sid string
			var n int
			if crows.Scan(&sid, &n) == nil {
				counts[sid] = n
			}
		}
		crows.Close()
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, model sql.NullString
		var parentID sql.NullString
		var cost float64
		var in, out, rea, cr, cw sql.NullInt64
		if err := rows.Scan(&id, &parentID, &model, &cost, &in, &out, &rea, &cr, &cw); err != nil {
			continue
		}
		t := Totals{
			Input:      int(in.Int64),
			Output:     int(out.Int64),
			Reasoning:  int(rea.Int64),
			CacheRead:  int(cr.Int64),
			CacheWrite: int(cw.Int64),
			Cost:       cost,
			Requests:   counts[id.String],
		}
		if parentID.Valid && parentID.String != "" {
			// child session: attribute to subagents bucket
			rep.Subagents.Add(t)
		} else {
			rep.Root.Add(t)
		}
		if model.Valid && model.String != "" {
			key := modelKey(model.String)
			rep.ByModel[key] = appendTotals(rep.ByModel[key], t)
		}
	}
	rep.Total.Requests = rep.Root.Requests + rep.Subagents.Requests
	rep.Total.Input = rep.Root.Input + rep.Subagents.Input
	rep.Total.Output = rep.Root.Output + rep.Subagents.Output
	rep.Total.Reasoning = rep.Root.Reasoning + rep.Subagents.Reasoning
	rep.Total.CacheRead = rep.Root.CacheRead + rep.Subagents.CacheRead
	rep.Total.CacheWrite = rep.Root.CacheWrite + rep.Subagents.CacheWrite
	rep.Total.Cost = rep.Root.Cost + rep.Subagents.Cost
	return rep, nil
}

func appendTotals(dst, t Totals) Totals {
	dst.Add(t)
	return dst
}

// modelKey converts a persisted model descriptor (JSON) into provider/model.
func modelKey(raw string) string {
	var m struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err == nil && m.ID != "" {
		if m.ProviderID != "" {
			return m.ProviderID + "/" + m.ID
		}
		return m.ID
	}
	return raw
}

// Record is a single request row from the message table, used to attribute
// per-request accounting to models.
type Record struct {
	Model string
	Totals
}

// RecentRecords returns assistant messages for the session tree since a time,
// used to reconcile rate-limit reservations with actual token usage.
func RecentRecords(root string, since time.Time, limit int) ([]Record, error) {
	if root == "" {
		return nil, nil
	}
	db, _, err := open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ids, err := sessionsFor(db, root)
	if err != nil {
		return nil, err
	}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids)+2)
	for id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	args = append(args, since.UnixMilli(), limit)
	rows, err := db.Query(fmt.Sprintf(`
		SELECT data FROM session_message
		WHERE session_id IN (%s) AND type = 'assistant'
		  AND time_created >= ?
		ORDER BY time_created DESC LIMIT ?`,
		strings.Join(placeholders, ",")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		rec, ok := parseMessage(raw)
		if ok {
			out = append(out, rec)
		}
	}
	return out, nil
}

// rawMessage mirrors the persisted assistant message payload.
type rawMessage struct {
	Model *struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
	} `json:"model"`
	Cost   float64 `json:"cost"`
	Tokens *struct {
		Input     int `json:"input"`
		Output    int `json:"output"`
		Reasoning int `json:"reasoning"`
		Cache     *struct {
			Read  int `json:"read"`
			Write int `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

func parseMessage(raw string) (Record, bool) {
	var m rawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m.Tokens == nil {
		return Record{}, false
	}
	rec := Record{
		Totals: Totals{
			Input:     m.Tokens.Input,
			Output:    m.Tokens.Output,
			Reasoning: m.Tokens.Reasoning,
			CacheRead: 0,
			Cost:      m.Cost,
		},
	}
	if m.Tokens.Cache != nil {
		rec.CacheRead = m.Tokens.Cache.Read
		rec.CacheWrite = m.Tokens.Cache.Write
	}
	if m.Model != nil {
		rec.Model = m.Model.ProviderID + "/" + m.Model.ID
	}
	return rec, true
}
