// Package gateway runs a loopback OpenAI-compatible proxy that sits between
// the engine and the upstream provider. Every request is admitted against the
// warden rate-limit windows BEFORE it reaches the provider, and actual token
// usage is reconciled afterwards.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/esdras-schonevald/open-warden/internal/limiter"
	"github.com/esdras-schonevald/open-warden/internal/registry"
)

// Server is the local request gateway.
type Server struct {
	URL   string
	Token string

	providers map[string]registry.Provider
	lim       *limiter.Limiter
	maxWait   time.Duration
	headroom  int
	maxOutput int
	maxInput  int
	capped    int
	http      *http.Client
	ln        net.Listener
	srv       *http.Server
	debug     bool

	mu       sync.Mutex
	requests int
}

// Options tunes gateway enforcement.
type Options struct {
	MaxWait   time.Duration
	Headroom  int
	MaxOutput int
	MaxInput  int
}

// New starts a gateway bound to a random loopback port.
func New(providers map[string]registry.Provider, lim *limiter.Limiter, opts Options) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("gateway listen: %w", err)
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		ln.Close()
		return nil, err
	}
	if opts.MaxWait <= 0 {
		opts.MaxWait = 20 * time.Second
	}
	if opts.Headroom <= 0 {
		opts.Headroom = 4096
	}
	s := &Server{
		URL:       "http://" + ln.Addr().String(),
		Token:     hex.EncodeToString(raw),
		providers: providers,
		lim:       lim,
		maxWait:   opts.MaxWait,
		headroom:  opts.Headroom,
		maxOutput: opts.MaxOutput,
		maxInput:  opts.MaxInput,
		http: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: time.Second,
			},
		},
		ln:    ln,
		debug: os.Getenv("WARDEN_GATEWAY_DEBUG") != "",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		_ = s.srv.Serve(ln)
	}()
	return s, nil
}

// BaseURL returns the base URL the engine must use for a provider.
func (s *Server) BaseURL(provider string) string {
	return s.URL + "/" + provider + "/v1"
}

// Close stops the gateway.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

// Capped reports how many requests had their output cap applied.
func (s *Server) Capped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.capped
}

// Requests reports how many proxied requests the gateway handled.
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("x-warden-gateway")), []byte(s.Token)) != 1 {
		writeError(w, http.StatusUnauthorized, "gateway_auth", "missing or invalid gateway token")
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(parts) < 1 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not_found", "unknown provider path")
		return
	}
	providerID := parts[0]
	prov, ok := s.providers[providerID]
	if !ok || prov.Upstream == "" {
		writeError(w, http.StatusNotFound, "provider_not_found", "provider is not routed through warden")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "could not read request body")
		return
	}
	model, stream, streamOpts := parseRequest(body)
	if model == "" && r.Method != http.MethodGet {
		writeError(w, http.StatusBadRequest, "invalid_request", "missing model")
		return
	}
	if stream {
		body = ensureStreamUsage(body, streamOpts)
	}
	if s.maxOutput > 0 && model != "" {
		var capped bool
		body, capped = applyOutputCap(body, r.URL.Path, prov.Protocol, s.maxOutput)
		if capped {
			s.mu.Lock()
			s.capped++
			s.mu.Unlock()
		}
	}

	est := 0
	var reservedAt time.Time
	if model != "" {
		if limit := s.maxInput; limit > 0 && estimateInput(body) > limit {
			writeError(w, http.StatusRequestEntityTooLarge, "context_overflow",
				"request exceeds the configured input token limit; reduce the context or enable compaction")
			return
		}
		if ctxLen := registry.ContextFor(limitKey(providerID, model)); ctxLen > 0 && estimateInput(body) > ctxLen {
			writeError(w, http.StatusRequestEntityTooLarge, "context_overflow",
				"request exceeds the model context window; reduce the context or enable compaction")
			return
		}
		est = estimateTokens(body, s.headroom)
		admitted, at := s.admit(r.Context(), providerID, model, est)
		if !admitted {
			writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded",
				"warden rate limit reached; retry later or use a fallback model")
			s.logf("provider=%s model=%s est=%d status=429", providerID, model, est)
			return
		}
		reservedAt = at
	}

	target := joinUpstream(prov.Upstream, strings.TrimPrefix(r.URL.Path, "/"+providerID))
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "could not build upstream request")
		return
	}
	copyHeaders(req.Header, r.Header)
	req.Header.Del("x-warden-gateway")
	req.Header.Del("Content-Length")
	req.ContentLength = int64(len(body))
	if req.Header.Get("Authorization") == "" {
		for _, env := range prov.Env {
			if v := os.Getenv(env); v != "" {
				req.Header.Set("Authorization", "Bearer "+v)
				break
			}
		}
	}

	resp, err := s.http.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.Header().Del("Content-Encoding")
	w.WriteHeader(resp.StatusCode)

	var actual Tokens
	if stream || strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		actual = s.copyStream(w, resp.Body)
	} else {
		raw, _ := io.ReadAll(resp.Body)
		w.Write(raw)
		if t, ok := extractUsage(raw); ok {
			actual = t
		}
	}
	if actual.Total() > 0 && model != "" {
		s.lim.Adjust(limitKey(providerID, model), reservedAt, actual.Total())
	}
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()
	s.logf("provider=%s model=%s est=%d actual=%d status=%d", providerID, model, est, actual.Total(), resp.StatusCode)
}

// admit checks the sliding windows and waits briefly for an opening instead of
// failing a request that is only marginally over a burst limit.
func (s *Server) admit(ctx context.Context, providerID, model string, est int) (bool, time.Time) {
	key := limitKey(providerID, model)
	deadline := time.Now().Add(s.maxWait)
	for {
		a := s.lim.Admit(key, est)
		if a.Allowed {
			return true, s.lim.Consume(key, est)
		}
		now := time.Now()
		if now.After(deadline) {
			return false, time.Time{}
		}
		wait := a.WaitFor
		if wait <= 0 {
			wait = 250 * time.Millisecond
		}
		if remaining := deadline.Sub(now); wait > remaining {
			wait = remaining
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return false, time.Time{}
		}
	}
}

// limitKey normalizes an upstream model reference to the warden model key
// (provider/model), dropping any variant suffix.
func limitKey(providerID, model string) string {
	return providerID + "/" + strings.TrimSpace(model)
}

// Tokens is the normalized accounting payload from an upstream response.
type Tokens struct {
	Input      int
	Output     int
	Reasoning  int
	CacheRead  int
	CacheWrite int
}

// Total returns the billable token weight.
func (t Tokens) Total() int {
	return t.Input + t.Output + t.Reasoning + t.CacheRead + t.CacheWrite
}

type rawUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadInput      int `json:"cache_read_input_tokens"`
	CacheCreationInput  int `json:"cache_creation_input_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func normalize(u rawUsage) (Tokens, bool) {
	in := u.PromptTokens
	if in == 0 {
		in = u.InputTokens
	}
	out := u.CompletionTokens
	if out == 0 {
		out = u.OutputTokens
	}
	cache := u.PromptTokensDetails.CachedTokens
	if cache == 0 {
		cache = u.CacheReadInput
	}
	reason := u.CompletionTokensDetails.ReasoningTokens
	write := u.CacheCreationInput
	t := Tokens{Input: in, Output: out, Reasoning: reason, CacheRead: cache, CacheWrite: write}
	return t, t.Total() > 0
}

func extractUsage(raw []byte) (Tokens, bool) {
	var doc struct {
		Usage   *rawUsage `json:"usage"`
		Message *struct {
			Usage *rawUsage `json:"usage"`
		} `json:"message"`
		Response *struct {
			Usage *rawUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return Tokens{}, false
	}
	if doc.Usage != nil {
		return normalize(*doc.Usage)
	}
	if doc.Message != nil && doc.Message.Usage != nil {
		return normalize(*doc.Message.Usage)
	}
	if doc.Response != nil && doc.Response.Usage != nil {
		return normalize(*doc.Response.Usage)
	}
	return Tokens{}, false
}

// copyStream forwards SSE chunks and extracts usage from the final data frames.
func (s *Server) copyStream(w http.ResponseWriter, r io.Reader) Tokens {
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReaderSize(r, 64*1024)
	var actual Tokens
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
				if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
					if t, ok := extractUsage(payload); ok {
						actual = mergeTokens(actual, t)
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	return actual
}

// mergeTokens combines usage fragments from streamed protocols (Anthropic
// reports input usage in message_start and final output in message_delta).
func mergeTokens(dst, src Tokens) Tokens {
	if src.Input > 0 {
		dst.Input = src.Input
	}
	if src.Output > 0 {
		dst.Output = src.Output
	}
	if src.Reasoning > 0 {
		dst.Reasoning = src.Reasoning
	}
	if src.CacheRead > 0 {
		dst.CacheRead = src.CacheRead
	}
	if src.CacheWrite > 0 {
		dst.CacheWrite = src.CacheWrite
	}
	return dst
}

func parseRequest(body []byte) (model string, stream bool, streamOpts map[string]any) {
	var doc struct {
		Model         string         `json:"model"`
		Stream        bool           `json:"stream"`
		StreamOptions map[string]any `json:"stream_options"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return "", false, nil
	}
	return doc.Model, doc.Stream, doc.StreamOptions
}

// ensureStreamUsage asks OpenAI-compatible upstreams to include a final usage
// frame. Providers that do not understand the field ignore it.
func ensureStreamUsage(body []byte, opts map[string]any) []byte {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return body
	}
	if opts == nil {
		opts = map[string]any{}
	}
	if _, ok := opts["include_usage"]; ok {
		return body
	}
	opts["include_usage"] = true
	doc["stream_options"] = opts
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

// estimateInput estimates only the prompt weight (no output headroom).
func estimateInput(body []byte) int {
	return len(body)/4 + 256
}

// applyOutputCap lowers max_tokens/max_completion_tokens/max_output_tokens to
// the configured cap. It never raises a value already chosen by the caller.
func applyOutputCap(body []byte, path string, protocol registry.Protocol, cap int) ([]byte, bool) {
	if cap <= 0 {
		return body, false
	}
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return body, false
	}
	field := "max_tokens"
	switch {
	case protocol == registry.Anthropic:
		field = "max_tokens"
	case strings.Contains(path, "/responses"):
		field = "max_output_tokens"
	default:
		if _, ok := doc["max_completion_tokens"]; ok {
			field = "max_completion_tokens"
		}
	}
	if cur, ok := doc[field].(float64); ok && cur > 0 && int(cur) <= cap {
		return body, false
	}
	doc[field] = cap
	out, err := json.Marshal(doc)
	if err != nil {
		return body, false
	}
	return out, true
}

// estimateTokens is deliberately conservative: JSON payload size plus a
// reasoning/output headroom.
func estimateTokens(body []byte, headroom int) int {
	if headroom <= 0 {
		headroom = 4096
	}
	est := len(body)/4 + headroom
	if est < 1 {
		est = 1
	}
	return est
}

func joinUpstream(base, suffix string) string {
	base = strings.TrimRight(base, "/")
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(suffix, "/v1") {
		suffix = strings.TrimPrefix(suffix, "/v1")
	}
	return base + suffix
}

var hopHeaders = map[string]bool{
	"connection":          true,
	"proxy-connection":    true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		lk := strings.ToLower(k)
		if hopHeaders[lk] || lk == "host" || lk == "content-length" {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func writeError(w http.ResponseWriter, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"type": kind, "message": msg},
	})
}

func (s *Server) logf(format string, args ...any) {
	if !s.debug {
		return
	}
	fmt.Fprintf(os.Stderr, "warden-gateway: "+format+"\n", args...)
}
