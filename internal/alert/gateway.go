// gateway.go implements the AlertGateway: an HTTP server that receives
// alerts from multiple sources via registered Adapters, normalises them,
// applies dedup/aggregation/silencing and forwards the survivors to a
// downstream AlertHandler.
//
// HTTP endpoints:
//
//	POST /webhook/{adapter}   — receive a raw payload for the adapter
//	GET  /healthz             — liveness probe
//	GET  /alerts              — list recently received alerts (best-effort)
//	GET  /silences            — list active silence rules
//	POST /silences            — add a silence rule (JSON body)
//	DELETE /silences/{id}     — remove a silence rule
//
// The gateway is concurrency-safe.
package alert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// Adapter is the interface every source adapter must implement.
type Adapter interface {
	// Name returns the adapter identifier, e.g. "prometheus" or "custom".
	Name() string
	// Parse converts a raw webhook payload into a slice of unified Alerts.
	Parse(raw []byte) ([]*Alert, error)
	// Validate checks the raw payload without converting it.
	Validate(raw []byte) error
}

// AlertHandler is the downstream sink for processed alerts. Implementations
// typically forward to a notification channel, an event bus or a durable
// store. The context carries request-scoped deadlines.
type AlertHandler interface {
	// HandleAlert processes a single alert. The implementation must be
	// concurrency-safe.
	HandleAlert(ctx context.Context, alert *Alert) error
}

// AlertHandlerFunc is the function adapter for AlertHandler.
type AlertHandlerFunc func(ctx context.Context, alert *Alert) error

// HandleAlert satisfies AlertHandler.
func (f AlertHandlerFunc) HandleAlert(ctx context.Context, a *Alert) error {
	return f(ctx, a)
}

// GatewayConfig configures an AlertGateway.
type GatewayConfig struct {
	// Addr is the listen address, e.g. ":9095".
	Addr string
	// ReadHeaderTimeout is forwarded to http.Server. Defaults to 10s.
	ReadHeaderTimeout time.Duration
	// MaxBodyBytes limits the size of a single webhook payload. Defaults to
	// 1 MiB.
	MaxBodyBytes int64
	// Dedup is the deduplication window. Zero disables dedup.
	Dedup time.Duration
	// Aggregate is the aggregation window. Zero disables aggregation.
	Aggregate time.Duration
}

// AlertGateway is the alert HTTP gateway. Construct with NewAlertGateway and
// start with Start.
type AlertGateway struct {
	cfg        GatewayConfig
	adapters   map[string]Adapter
	deduper    *Deduper
	aggregator *Aggregator
	silencer   *Silencer
	handler    AlertHandler

	mu       sync.RWMutex
	recent   []*Alert // ring of recently received alerts (best-effort)
	recentMu sync.Mutex

	server *http.Server
}

// NewAlertGateway constructs an AlertGateway. The AlertHandler may be nil; in
// that case alerts are accepted but discarded (useful for testing).
func NewAlertGateway(cfg GatewayConfig, handler AlertHandler) *AlertGateway {
	if cfg.ReadHeaderTimeout == 0 {
		cfg.ReadHeaderTimeout = 10 * time.Second
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = 1 << 20 // 1 MiB
	}
	g := &AlertGateway{
		cfg:      cfg,
		adapters: make(map[string]Adapter),
		silencer: NewSilencer(),
		handler:  handler,
	}
	if cfg.Dedup > 0 {
		g.deduper = NewDeduper(DeduperConfig{Window: cfg.Dedup})
	}
	if cfg.Aggregate > 0 {
		g.aggregator = NewAggregator(AggregatorConfig{Window: cfg.Aggregate}, nil)
	}
	return g
}

// RegisterAdapter registers an Adapter under its Name(). Re-registering the
// same name overwrites the previous adapter.
func (g *AlertGateway) RegisterAdapter(a Adapter) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.adapters[a.Name()] = a
}

// AdapterNames returns the names of all registered adapters, sorted.
func (g *AlertGateway) AdapterNames() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	names := make([]string, 0, len(g.adapters))
	for n := range g.adapters {
		names = append(names, n)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return names
}

// Silencer returns the gateway's silencer so callers can manage rules
// programmatically.
func (g *AlertGateway) Silencer() *Silencer { return g.silencer }

// Deduper returns the gateway's deduper (may be nil when dedup is disabled).
func (g *AlertGateway) Deduper() *Deduper { return g.deduper }

// Aggregator returns the gateway's aggregator (may be nil when aggregation
// is disabled).
func (g *AlertGateway) Aggregator() *Aggregator { return g.aggregator }

// Start binds the listener and blocks until ctx is cancelled or the server
// errors out. The HTTP mux is built once and reused across restarts.
func (g *AlertGateway) Start(ctx context.Context, addr string) error {
	if addr == "" {
		addr = g.cfg.Addr
	}
	if addr == "" {
		return errors.New("alert gateway: addr is required")
	}
	mux := g.buildMux()
	g.server = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: g.cfg.ReadHeaderTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := g.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	log.Info("alert gateway listening", "addr", addr, "adapters", strings.Join(g.AdapterNames(), ","))
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return g.server.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// Stop gracefully shuts the server down. It is safe to call when the server
// is not running.
func (g *AlertGateway) Stop() {
	if g.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = g.server.Shutdown(ctx)
}

// buildMux wires the HTTP routes.
func (g *AlertGateway) buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", g.handleHealthz)
	mux.HandleFunc("/webhook/", g.handleWebhook)
	mux.HandleFunc("/alerts", g.handleListAlerts)
	mux.HandleFunc("/silences", g.handleSilences)
	mux.HandleFunc("/silences/", g.handleSilenceByID)
	return mux
}

// Handler returns an http.Handler for the gateway. It is intended for tests
// and for embedding the gateway in an existing HTTP server via
// httptest.NewServer(g.Handler()). The handler is built fresh on every call
// but shares the gateway's underlying state.
func (g *AlertGateway) Handler() http.Handler {
	return g.buildMux()
}

// handleHealthz is the liveness probe.
func (g *AlertGateway) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"adapters": g.AdapterNames(),
	})
}

// handleWebhook is the main entry point: /webhook/{adapter}.
func (g *AlertGateway) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	adapterName := strings.TrimPrefix(r.URL.Path, "/webhook/")
	if adapterName == "" || strings.Contains(adapterName, "/") {
		writeError(w, http.StatusBadRequest, "missing adapter name")
		return
	}
	g.mu.RLock()
	adapter, ok := g.adapters[adapterName]
	g.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown adapter %q", adapterName))
		return
	}

	body, err := readBody(r, g.cfg.MaxBodyBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := adapter.Validate(body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	alerts, err := adapter.Parse(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	results := make([]map[string]any, 0, len(alerts))
	for _, a := range alerts {
		status, msg := g.processAlert(r.Context(), a)
		results = append(results, map[string]any{
			"id":     a.ID,
			"status": status,
			"reason": msg,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"adapter": adapterName,
		"count":   len(alerts),
		"results": results,
	})
}

// processAlert applies the full pipeline (validate -> dedup -> silence ->
// aggregate -> dispatch) to a single alert. It returns a short status string
// ("accepted" | "duplicate" | "silenced" | "invalid" | "error") and a
// human-readable reason.
func (g *AlertGateway) processAlert(ctx context.Context, a *Alert) (string, string) {
	if err := a.Validate(); err != nil {
		log.Warn("alert rejected: invalid", "err", err, "alert", a.String())
		return "invalid", err.Error()
	}

	if g.deduper != nil {
		if !g.deduper.CheckAndMark(a.Fingerprint) {
			log.Debug("alert rejected: duplicate", "id", a.ID)
			return "duplicate", ErrDuplicateAlert.Error()
		}
	}

	if silenced, ruleID, reason := g.silencer.IsSilenced(a); silenced {
		log.Info("alert silenced", "id", a.ID, "rule", ruleID, "reason", reason)
		return "silenced", fmt.Sprintf("%s: rule=%s", ErrSilenced.Error(), ruleID)
	}

	g.recordRecent(a)

	if g.aggregator != nil {
		if _, err := g.aggregator.Add(ctx, a); err != nil {
			log.Warn("aggregator add failed", "err", err)
		}
	}

	if g.handler != nil {
		if err := g.handler.HandleAlert(ctx, a); err != nil {
			log.Error("handler dispatch failed", "err", err, "id", a.ID)
			return "error", err.Error()
		}
	}
	log.Debug("alert accepted", "id", a.ID, "source", a.Source)
	return "accepted", ""
}

// recordRecent appends the alert to the bounded recent ring. The ring is
// capped at 256 entries; older entries are evicted FIFO.
func (g *AlertGateway) recordRecent(a *Alert) {
	g.recentMu.Lock()
	defer g.recentMu.Unlock()
	g.recent = append(g.recent, a)
	if len(g.recent) > 256 {
		g.recent = g.recent[len(g.recent)-256:]
	}
}

// RecentAlerts returns a snapshot of the recently received alerts. The
// returned slice is a copy and may be mutated safely.
func (g *AlertGateway) RecentAlerts() []*Alert {
	g.recentMu.Lock()
	defer g.recentMu.Unlock()
	out := make([]*Alert, len(g.recent))
	copy(out, g.recent)
	return out
}

// handleListAlerts returns the recently received alerts as JSON.
func (g *AlertGateway) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	alerts := g.RecentAlerts()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"count":  len(alerts),
		"alerts": alerts,
	})
}

// handleSilences dispatches GET (list) and POST (add) on /silences.
func (g *AlertGateway) handleSilences(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rules := g.silencer.ListRules()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": len(rules),
			"rules": rules,
		})
	case http.MethodPost:
		body, err := readBody(r, g.cfg.MaxBodyBytes)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var rule SilenceRule
		if err := json.Unmarshal(body, &rule); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
			return
		}
		id := g.silencer.AddRule(rule)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleSilenceByID handles DELETE /silences/{id}.
func (g *AlertGateway) handleSilenceByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/silences/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing silence id")
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if !g.silencer.RemoveRule(id) {
			writeError(w, http.StatusNotFound, "silence rule not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		rule, err := g.silencer.GetRule(id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rule)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// readBody reads r.Body up to maxBytes and returns the bytes. It returns an
// error when the body is empty or exceeds maxBytes.
func readBody(r *http.Request, maxBytes int64) ([]byte, error) {
	if r.Body == nil {
		return nil, errors.New("empty body")
	}
	reader := http.MaxBytesReader(nil, r.Body, maxBytes)
	b, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(b) == 0 {
		return nil, errors.New("empty body")
	}
	return b, nil
}

// writeError emits a JSON error envelope.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})
}
