package gateway

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/esdras-schonevald/open-warden/internal/config"
	"github.com/esdras-schonevald/open-warden/internal/limiter"
)

func testServer(t *testing.T, upstream string, rpm int) (*Server, *limiter.Limiter) {
	t.Helper()
	cfg := &config.Config{
		Providers: map[string]config.Provider{"prov": {Upstream: upstream}},
		Gateway:   config.GatewayOptions{MaxWait: 50 * time.Millisecond},
		Limits:    &config.Limits{RPM: rpm},
		Run:       config.RunOptions{OutputHeadroom: 100},
	}
	lim := limiter.New(cfg.Limits, nil)
	s, err := New(cfg, lim)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, lim
}

func do(t *testing.T, s *Server, body string, header map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, s.BaseURL("prov")+"/chat/completions", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-warden-gateway", s.Token)
	req.Header.Set("authorization", "Bearer upstream-key")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestGatewayForwardsAndAccountsUsage(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"x","usage":{"prompt_tokens":12,"completion_tokens":3,"completion_tokens_details":{"reasoning_tokens":2}},"choices":[]}`)
	}))
	defer up.Close()

	s, lim := testServer(t, up.URL, 10)
	resp := do(t, s, `{"model":"coder","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path %q", gotPath)
	}
	if gotAuth != "Bearer upstream-key" {
		t.Fatalf("auth not forwarded: %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"coder"`) {
		t.Fatalf("body not forwarded: %s", gotBody)
	}
	// prompt 12 + completion 3 + reasoning 2 = 17
	win := lim.Snapshot()["prov/coder"]
	if win == nil || len(win.Tokens) != 1 || win.Tokens[0].Tokens != 17 {
		t.Fatalf("usage not reconciled: %+v", win)
	}
}

func TestGatewayRejectsOverLimit(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	s, _ := testServer(t, up.URL, 1)
	if resp := do(t, s, `{"model":"m","messages":[{"role":"user","content":"a"}]}`, nil); resp.StatusCode != 200 {
		t.Fatalf("first status %d", resp.StatusCode)
	}
	resp := do(t, s, `{"model":"m","messages":[{"role":"user","content":"b"}]}`, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second status %d, want 429", resp.StatusCode)
	}
}

func TestGatewayRequiresToken(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s, _ := testServer(t, up.URL, 10)
	req, _ := http.NewRequest(http.MethodPost, s.BaseURL("prov")+"/chat/completions", bytes.NewReader([]byte(`{"model":"m"}`)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
}

func TestGatewayStreamsAndParsesUsage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	s, lim := testServer(t, up.URL, 10)
	resp := do(t, s, `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`, nil)
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "[DONE]") || !strings.Contains(string(body), `"content":"hi"`) {
		t.Fatalf("stream not forwarded: %s", body)
	}
	win := lim.Snapshot()["prov/m"]
	if win == nil || len(win.Tokens) != 1 || win.Tokens[0].Tokens != 13 {
		t.Fatalf("stream usage not applied: %+v", win)
	}
}
