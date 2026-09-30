// rest_gate.go — the GateService: ad-hoc single-step verification over
// REST (POST /gates/verify).
//
// Design rationale. The engine already materialises declared gates from
// the plan artifact and runs them at phase boundaries — that path is the
// audit-bound, hash-locked execution of an APPROVED plan. What it does
// not offer is a way for an operator (or a pipeline pre-check, or the
// ChatOps "run this one health check now" flow) to execute one
// verification step on demand without planning a change around it.
// This service fills exactly that gap, deliberately:
//
//   - the check spec arrives in the request body (type + params +
//     optional target), is compiled with the SAME verify.Gate
//     constructors the engine uses (no re-implementation, no drift),
//     executed once against a fresh channel when a target is given,
//     and the result returned with its structured details;
//   - cmd/probe/slo only. "human" is out of scope by design: a blocking
//     approval checkpoint is not a single-step verification, it is
//     the approval chain (see the ChangeService kickoff / decide
//     flows). SLO checks require the gateway's Prometheus URL and fail
//     closed without it, mirroring the engine's materialisation rule;
//   - every execution is audited (action "gate_verify") with the run
//     association when the caller supplies one, so ad-hoc checks are
//     not an audit blind spot;
//   - the service is optional on the gateway (SetGateService); a
//     gateway without it answers 404, preserving zero-config startup.

package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/verify"
)

// GateService executes ad-hoc single-step verifications. It holds the
// same dependencies the engine's gate materialisation needs (a channel
// dialer for cmd checks against remote targets, the Prometheus URL for
// slo checks) plus the store for target lookup and audit.
type GateService struct {
	store state.Store
	// dialer opens a connected channel to an inventory host. The
	// production wiring (serve) injects a closure over the engine's
	// channel registry + credential resolution; a nil dialer makes cmd
	// checks against targets fail closed with an explicit error.
	dialer        ChannelDialer
	prometheusURL string
}

// ChannelDialer opens a connected channel to the named inventory host.
// The returned channel is closed by the caller after the check.
type ChannelDialer func(ctx context.Context, host string) (channel.Channel, error)

// NewGateService constructs a GateService. store is required (audit +
// target lookup); the dialer and Prometheus URL are optional — cmd
// checks against remote targets fail closed without a dialer, and slo
// checks fail closed without a URL, both with explicit errors.
func NewGateService(store state.Store, dialer ChannelDialer, prometheusURL string) *GateService {
	return &GateService{store: store, dialer: dialer, prometheusURL: prometheusURL}
}

// GateVerifyRequest is the JSON body of POST /gates/verify.
type GateVerifyRequest struct {
	// Type is the check type: "cmd" | "probe" | "slo". "human" is
	// rejected — the approval chain owns that flow.
	Type string `json:"type"`

	// Name is an optional label for the gate (defaults to "ad-hoc").
	Name string `json:"name,omitempty"`

	// Target is an optional inventory hostname. Cmd checks execute over
	// a channel to that target; probe/slo checks ignore it (probes may
	// carry their target inside params, e.g. host_port).
	Target string `json:"target,omitempty"`

	// RunID optionally associates the ad-hoc check with a run for
	// audit purposes. No gate semantics are derived from it.
	RunID string `json:"run_id,omitempty"`

	// Params is the gate's parameter map, exactly as the engine's
	// inline gate declarations carry it. The relevant keys per type:
	//   cmd:   {"cmd": "...", "expect_exit": 0, "expect_stdout": "...",
	//           "timeout_seconds": 5}
	//   probe: {"kind": "http|tcp|script", ...} (self-describing)
	//   slo:   {"query": "...", "threshold": 0.01,
	//           "comparison": "lte", "timeout_seconds": 5}
	Params map[string]any `json:"params"`
}

// GateVerifyResponse is the JSON result of POST /gates/verify.
type GateVerifyResponse struct {
	// Gate is the resolved gate name (request Name or "ad-hoc").
	Gate string `json:"gate"`

	// Type echoes the check type.
	Type string `json:"type"`

	// Target echoes the target when one was supplied.
	Target string `json:"target,omitempty"`

	// Passed is true when the check ran and succeeded.
	Passed bool `json:"passed"`

	// Message is the human-readable outcome (pass confirmation or
	// failure explanation).
	Message string `json:"message"`

	// Details is the gate's structured evidence (command output, probe
	// response, metric values).
	Details map[string]any `json:"details,omitempty"`

	// LatencyMs is the wall-clock duration of the check.
	LatencyMs int64 `json:"latency_ms"`

	// RanAt is the execution timestamp (UTC).
	RanAt time.Time `json:"ran_at"`
}

// Verify executes one ad-hoc gate check. The compile step reuses the
// engine-grade verify constructors; unknown types, invalid params and
// missing runtime dependencies (channel factory, Prometheus URL) fail
// with explicit errors — never a silent pass.
func (s *GateService) Verify(ctx context.Context, req *GateVerifyRequest) (*GateVerifyResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("gate verify: empty request")
	}
	name := req.Name
	if name == "" {
		name = "ad-hoc"
	}

	g, err := s.compileGate(name, req)
	if err != nil {
		return nil, err
	}

	input := verify.GateInput{Params: req.Params}
	var cleanup func()
	if req.Target != "" {
		// Cmd checks execute over a channel to the target (the verify
		// CommandGate requires one — an ad-hoc remote check, by design;
		// probe checks carry their own target inside params). Probe/slo
		// ignore the Target field for dialing.
		if req.Type == "cmd" {
			ch, done, err := s.dialTarget(ctx, req.Target)
			if err != nil {
				return nil, err
			}
			input.Channel = ch
			cleanup = done
		}
	} else if req.Type == "cmd" {
		// A cmd check without a target would have no channel to run on;
		// the CommandGate fails closed with "channel is nil" anyway, so
		// refuse up front with an actionable message instead.
		return nil, fmt.Errorf("gate verify: cmd check requires a target (the command runs on the target over a channel; use a probe with kind \"script\" for local checks)")
	}
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()

	start := time.Now().UTC()
	result, err := g.Check(ctx, input)
	ranAt := time.Now().UTC()
	resp := &GateVerifyResponse{
		Gate:      name,
		Type:      req.Type,
		Target:    req.Target,
		RanAt:     ranAt,
		LatencyMs: time.Since(start).Milliseconds(),
	}
	if err != nil {
		resp.Passed = false
		resp.Message = fmt.Sprintf("gate could not run: %v", err)
	} else {
		resp.Passed = result.Passed
		resp.Message = result.Message
		resp.Details = result.Details
	}

	// Audit trail: ad-hoc checks must not be an audit blind spot. The
	// state row has no detail column, so the outcome and a truncated
	// message share the Result field.
	if s.store != nil {
		actor := actorFromCtx(ctx)
		target := req.Target
		if target == "" {
			target = "-"
		}
		outcome := "failed"
		if resp.Passed {
			outcome = "passed"
		}
		msg := resp.Message
		if len(msg) > 200 {
			msg = msg[:200]
		}
		_ = s.store.CreateAudit(ctx, &state.Audit{
			ID:        newID("aud-"),
			RunID:     req.RunID,
			Action:    "gate_verify",
			Actor:     actor,
			Target:    target,
			Result:    outcome + ": " + msg,
			Timestamp: ranAt,
		})
	}
	return resp, nil
}

// compileGate builds the verify.Gate for the request. The constructors
// are the engine's own (NewCommandGate / NewProbeGate / NewSLOGate), so
// an ad-hoc check behaves exactly like its planned counterpart.
func (s *GateService) compileGate(name string, req *GateVerifyRequest) (verify.Gate, error) {
	switch req.Type {
	case "cmd":
		cmd, _ := req.Params["cmd"].(string)
		if cmd == "" {
			return nil, fmt.Errorf("gate verify: cmd check requires a \"cmd\" param")
		}
		opts := []verify.CommandGateOption{}
		if v, ok := req.Params["expect_exit"]; ok {
			if code, ok := looseIntParam(v); ok {
				opts = append(opts, verify.WithExpectedExit(code))
			}
		}
		if v, ok := req.Params["expect_stdout"].(string); ok && v != "" {
			opts = append(opts, verify.WithExpectedStdout(v))
		}
		if v, ok := req.Params["timeout_seconds"]; ok {
			if secs, ok := looseIntParam(v); ok && secs > 0 {
				opts = append(opts, verify.WithCommandTimeout(time.Duration(secs)*time.Second))
			}
		}
		// Phase is irrelevant for a one-shot check; any legal phase
		// satisfies the constructor.
		return verify.NewCommandGate(name, verify.PhasePostApply, cmd, opts...), nil

	case "probe":
		params := req.Params
		if params == nil {
			params = map[string]any{}
		}
		return verify.NewProbeGate(name, verify.PhasePostApply, params), nil

	case "slo":
		query, _ := req.Params["query"].(string)
		if query == "" {
			return nil, fmt.Errorf("gate verify: slo check requires a \"query\" param")
		}
		if s.prometheusURL == "" {
			return nil, fmt.Errorf("gate verify: slo check requires the gateway's Prometheus URL (verify.prometheus_url); refusing to guess")
		}
		threshold, ok := looseFloatParam(req.Params["threshold"])
		if !ok {
			return nil, fmt.Errorf("gate verify: slo check requires a numeric \"threshold\" param")
		}
		comparison := "le"
		if cmp, _ := req.Params["comparison"].(string); cmp != "" {
			comparison = cmp
		}
		opts := []verify.SLOGateOption{verify.WithSLOSource(s.prometheusURL)}
		if v, ok := req.Params["timeout_seconds"]; ok {
			if secs, ok := looseIntParam(v); ok && secs > 0 {
				opts = append(opts, verify.WithSLOTimeout(time.Duration(secs)*time.Second))
			}
		}
		return verify.NewSLOGate(name, query, threshold, comparison, opts...), nil

	case "human":
		return nil, fmt.Errorf("gate verify: \"human\" is not a single-step check; use the approval chain (ApproveChange / mobile approval)")

	default:
		return nil, fmt.Errorf("gate verify: unknown check type %q (supported: cmd|probe|slo)", req.Type)
	}
}

// dialTarget validates the host against the inventory and opens a
// channel to it via the injected dialer. The returned cleanup closes
// the channel. Retired and unknown hosts are refused up front — an
// ad-hoc check must not silently become a way to reach decommissioned
// machines.
func (s *GateService) dialTarget(ctx context.Context, host string) (channel.Channel, func(), error) {
	noop := func() {}
	if s.store == nil {
		return nil, noop, fmt.Errorf("gate verify: no store configured to resolve target %q", host)
	}
	if s.dialer == nil {
		return nil, noop, fmt.Errorf("gate verify: no channel dialer configured (serve must wire the engine registry); refusing to dial %q", host)
	}
	rows, err := s.store.ListTargets(ctx, state.TargetFilter{})
	if err != nil {
		return nil, noop, fmt.Errorf("gate verify: list targets: %w", err)
	}
	var row *state.Target
	for i := range rows {
		if rows[i].Hostname == host {
			row = rows[i]
			break
		}
	}
	if row == nil {
		return nil, noop, fmt.Errorf("gate verify: target %q not found in inventory", host)
	}
	if row.Status == "retired" {
		return nil, noop, fmt.Errorf("gate verify: target %q is retired", host)
	}

	ch, err := s.dialer(ctx, host)
	if err != nil {
		return nil, noop, fmt.Errorf("gate verify: dial %q: %w", host, err)
	}
	return ch, func() { _ = ch.Close() }, nil
}

// looseIntParam coerces the plausible JSON/YAML numeric shapes to int.
func looseIntParam(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}

// looseFloatParam coerces the plausible numeric shapes to float64.
func looseFloatParam(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// --- REST endpoint -----------------------------------------------------------

// gateServiceHandler decouples the gateway from the concrete type.
type gateServiceHandler interface {
	Verify(ctx context.Context, req *GateVerifyRequest) (*GateVerifyResponse, error)
}

// SetGateService registers an ad-hoc gate verification service. When
// set, the gateway exposes POST /gates/verify; otherwise that path
// answers 404 like any unknown route.
func (gw *Gateway) SetGateService(g gateServiceHandler) {
	gw.gateService = g
}

// handleGateVerify implements POST /gates/verify.
func (gw *Gateway) handleGateVerify(w http.ResponseWriter, r *http.Request) {
	if gw.gateService == nil {
		writeJSONError(w, http.StatusNotFound, "gate service not configured")
		return
	}
	var req GateVerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	resp, err := gw.gateService.Verify(r.Context(), &req)
	if err != nil {
		// Bad requests (unknown type, missing params, missing runtime
		// dependency) are 400s, not 500s: the caller described a check
		// the system honestly cannot execute.
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
