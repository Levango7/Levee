// step_gates_test.go — tests for compiling inline workflow gate
// declarations (plan.PlanStep.Gate) into registered verify.Gate instances.
package engine

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/verify"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubGateChannel records executed commands and returns canned results.
type stubGateChannel struct {
	exitCode int
	stdout   string
}

func (s *stubGateChannel) Connect(_ context.Context) error { return nil }
func (s *stubGateChannel) Close() error                    { return nil }
func (s *stubGateChannel) Exec(_ context.Context, _ string) (*channel.ExecResult, error) {
	return &channel.ExecResult{ExitCode: s.exitCode, Stdout: s.stdout}, nil
}
func (s *stubGateChannel) Upload(_ context.Context, _ string, _ io.Reader) error {
	return nil
}
func (s *stubGateChannel) Download(_ context.Context, _ string) (io.Reader, error) {
	return nil, nil
}
func (s *stubGateChannel) IsConnected() bool { return true }

func TestMaterializeStepGates_NoDeclarations(t *testing.T) {
	gm := verify.NewGateManager()
	p := &plan.Plan{Batches: []plan.Batch{{Steps: []plan.PlanStep{{Name: "plain"}}}}}
	require.NoError(t, materializeStepGates(gm, p, GateRuntime{}))
	assert.Empty(t, gm.Gates(verify.PhasePreApply))
	assert.Empty(t, gm.Gates(verify.PhasePostApply))
	assert.Empty(t, gm.Gates(verify.PhasePostBatch))
}

func TestMaterializeStepGates_CmdCheckRegistersRunnableGate(t *testing.T) {
	gm := verify.NewGateManager()
	p := &plan.Plan{Batches: []plan.Batch{{
		Steps: []plan.PlanStep{{
			Name: "deploy",
			Gate: &dsl.GateSpec{
				Pre: []dsl.GateCheck{
					{Type: "cmd", Command: "systemctl is-active nginx", ExpectExit: 0},
				},
				Post: []dsl.GateCheck{
					{Type: "cmd", Command: "curl -fsS localhost/health", ExpectExit: 0, Timeout: "5s"},
				},
			},
		}},
	}}}
	require.NoError(t, materializeStepGates(gm, p, GateRuntime{}))

	pre := gm.Gates(verify.PhasePreApply)
	require.Len(t, pre, 1)
	post := gm.Gates(verify.PhasePostApply)
	require.Len(t, post, 1)

	// The pre gate must really execute through the channel and pass on a
	// matching exit code.
	res, err := pre[0].Check(context.Background(), verify.GateInput{
		RunID:   "run-1",
		Channel: &stubGateChannel{exitCode: 0},
	})
	require.NoError(t, err)
	assert.True(t, res.Passed)

	// ...and report a non-pass on a mismatching one (per the CommandGate
	// contract: command ran but did not match => Passed=false, err=nil).
	res, err = pre[0].Check(context.Background(), verify.GateInput{
		RunID:   "run-1",
		Channel: &stubGateChannel{exitCode: 3},
	})
	require.NoError(t, err)
	assert.False(t, res.Passed)
}

func TestMaterializeStepGates_UnsupportedTypeFailsClosed(t *testing.T) {
	gm := verify.NewGateManager()
	p := &plan.Plan{Batches: []plan.Batch{{
		Steps: []plan.PlanStep{{
			Name: "deploy",
			Gate: &dsl.GateSpec{
				Pre: []dsl.GateCheck{{Type: "telepathy", Command: "think hard"}},
			},
		}},
	}}}
	err := materializeStepGates(gm, p, GateRuntime{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not executable")
	assert.Empty(t, gm.Gates(verify.PhasePreApply), "nothing may be registered when materialisation fails")
}

// Runtime-dependent gates fail materialisation with explicit errors naming
// the missing configuration instead of registering a gate that could only
// guess or silently skip.

func TestMaterializeStepGates_SLOWithoutPrometheusURLErrors(t *testing.T) {
	gm := verify.NewGateManager()
	p := &plan.Plan{Batches: []plan.Batch{{
		Steps: []plan.PlanStep{{
			Name: "deploy",
			Gate: &dsl.GateSpec{
				Batch: []dsl.GateCheck{{
					Type:    "slo",
					Command: "rate(http_errors_total[5m])",
					Params:  map[string]any{"threshold": 0.01},
				}},
			},
		}},
	}}}
	err := materializeStepGates(gm, p, GateRuntime{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verify.prometheus_url")
	assert.Empty(t, gm.Gates(verify.PhasePostBatch))
}

func TestMaterializeStepGates_SLOWithRuntimeRegistersPostBatchGate(t *testing.T) {
	gm := verify.NewGateManager()
	p := &plan.Plan{Batches: []plan.Batch{{
		Steps: []plan.PlanStep{{
			Name: "deploy",
			Gate: &dsl.GateSpec{
				Batch: []dsl.GateCheck{{
					Type:    "slo",
					Command: "rate(http_errors_total[5m])",
					Params: map[string]any{
						"threshold":  0.01,
						"comparison": "lte",
					},
				}},
			},
		}},
	}}}
	rt := GateRuntime{PrometheusURL: "http://prom:9090"}
	require.NoError(t, materializeStepGates(gm, p, rt))

	batch := gm.Gates(verify.PhasePostBatch)
	require.Len(t, batch, 1)
	assert.Equal(t, "step:deploy:batch:0", batch[0].Name())

	// The gate must be a real SLOGate bound to post_batch and carrying the
	// configured source.
	sg, ok := batch[0].(*verify.SLOGate)
	require.True(t, ok)
	assert.Equal(t, verify.PhasePostBatch, sg.Phase())
}

func TestMaterializeStepGates_SLOUnknownComparisonFailsClosed(t *testing.T) {
	gm := verify.NewGateManager()
	p := &plan.Plan{Batches: []plan.Batch{{
		Steps: []plan.PlanStep{{
			Name: "deploy",
			Gate: &dsl.GateSpec{
				Batch: []dsl.GateCheck{{
					Type:    "slo",
					Command: "q",
					Params:  map[string]any{"threshold": 1, "comparison": "about-equal"},
				}},
			},
		}},
	}}}
	err := materializeStepGates(gm, p, GateRuntime{PrometheusURL: "http://prom:9090"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown comparison")
	assert.Empty(t, gm.Gates(verify.PhasePostBatch), "an ambiguous comparison must not silently coerce to lt")
}

func TestMaterializeStepGates_HumanWithoutApproverErrors(t *testing.T) {
	gm := verify.NewGateManager()
	p := &plan.Plan{Batches: []plan.Batch{{
		Steps: []plan.PlanStep{{
			Name: "deploy",
			Gate: &dsl.GateSpec{
				Post: []dsl.GateCheck{{Type: "human", Command: "please confirm"}},
			},
		}},
	}}}
	err := materializeStepGates(gm, p, GateRuntime{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `human gate "step:deploy:post:0" requires an approver`)
	assert.Empty(t, gm.Gates(verify.PhasePostApply))
}

func TestMaterializeStepGates_ProbeNeedsNoRuntimeAndRegisters(t *testing.T) {
	gm := verify.NewGateManager()
	p := &plan.Plan{Batches: []plan.Batch{{
		Steps: []plan.PlanStep{{
			Name: "deploy",
			Gate: &dsl.GateSpec{
				Post: []dsl.GateCheck{{
					Type: "probe",
					Params: map[string]any{
						"kind":            "tcp",
						"mode":            "direct",
						"host_port":       "127.0.0.1:1", // nothing listens here
						"timeout_seconds": 1,
					},
				}},
			},
		}},
	}}}
	// Zero runtime is fine for probes.
	require.NoError(t, materializeStepGates(gm, p, GateRuntime{}))
	post := gm.Gates(verify.PhasePostApply)
	require.Len(t, post, 1)

	res, err := post[0].Check(context.Background(), verify.GateInput{RunID: "run-1"})
	require.NoError(t, err)
	assert.False(t, res.Passed, "connecting to a closed port must fail the probe")
}

func TestMaterializeStepGates_BatchEntriesRegisterUnderPostBatchPhase(t *testing.T) {
	gm := verify.NewGateManager()
	p := &plan.Plan{Batches: []plan.Batch{{
		Steps: []plan.PlanStep{{
			Name: "deploy",
			Gate: &dsl.GateSpec{
				Batch: []dsl.GateCheck{
					{Type: "cmd", Command: "true"},
					{Type: "cmd", Command: "false", ExpectExit: 1},
				},
			},
		}},
	}}}
	require.NoError(t, materializeStepGates(gm, p, GateRuntime{}))

	batch := gm.Gates(verify.PhasePostBatch)
	require.Len(t, batch, 2)
	names := []string{batch[0].Name(), batch[1].Name()}
	assert.ElementsMatch(t, []string{"step:deploy:batch:0", "step:deploy:batch:1"}, names)
	for _, g := range batch {
		assert.Equal(t, verify.PhasePostBatch, g.Phase())
	}
}

func TestParseStrictParams(t *testing.T) {
	allowed := map[string]string{
		"name":    "string",
		"count":   "int",
		"ratio":   "float",
		"enabled": "bool",
	}

	t.Run("normalises loose numeric spellings", func(t *testing.T) {
		out, err := parseStrictParams(map[string]any{
			"name":    "x",
			"count":   float64(3),
			"ratio":   2,
			"enabled": true,
		}, allowed)
		require.NoError(t, err)
		assert.Equal(t, "x", out["name"])
		assert.Equal(t, 3, out["count"])
		assert.Equal(t, 2.0, out["ratio"])
		assert.Equal(t, true, out["enabled"])
	})

	t.Run("rejects unknown keys listing valid ones", func(t *testing.T) {
		_, err := parseStrictParams(map[string]any{"nope": 1}, allowed)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"nope"`)
		for _, k := range []string{"count", "enabled", "name", "ratio"} {
			assert.Contains(t, err.Error(), k)
		}
	})

	t.Run("rejects wrong types", func(t *testing.T) {
		_, err := parseStrictParams(map[string]any{"count": "three"}, allowed)
		require.Error(t, err)
		_, err = parseStrictParams(map[string]any{"ratio": "high"}, allowed)
		require.Error(t, err)
		_, err = parseStrictParams(map[string]any{"enabled": "yes"}, allowed)
		require.Error(t, err)
		_, err = parseStrictParams(map[string]any{"name": 7}, allowed)
		require.Error(t, err)
	})

	t.Run("empty params yield empty copy", func(t *testing.T) {
		out, err := parseStrictParams(nil, allowed)
		require.NoError(t, err)
		assert.Empty(t, out)
	})
}

func TestMaterializeStepGates_TimeoutParsed(t *testing.T) {
	gm := verify.NewGateManager()
	p := &plan.Plan{Batches: []plan.Batch{{
		Steps: []plan.PlanStep{{
			Name: "deploy",
			Gate: &dsl.GateSpec{
				Pre: []dsl.GateCheck{{Type: "cmd", Command: "true", Timeout: "250ms"}},
			},
		}},
	}}}
	require.NoError(t, materializeStepGates(gm, p, GateRuntime{}))
	pre := gm.Gates(verify.PhasePreApply)
	require.Len(t, pre, 1)

	start := time.Now()
	ch := &slowStubChannel{delay: 400 * time.Millisecond, base: stubGateChannel{exitCode: 0}}
	res, _ := pre[0].Check(context.Background(), verify.GateInput{RunID: "run-1", Channel: ch})
	// The attempt timed out (see the warn log); CommandGate surfaces an
	// exhausted retry budget as Passed=false with no terminal error.
	assert.False(t, res.Passed)
	assert.False(t, time.Since(start) < 200*time.Millisecond, "timeout must actually bound the attempt")
}

type slowStubChannel struct {
	base  stubGateChannel
	delay time.Duration
}

func (s *slowStubChannel) Connect(_ context.Context) error { return nil }
func (s *slowStubChannel) Close() error                    { return nil }
func (s *slowStubChannel) Exec(ctx context.Context, cmd string) (*channel.ExecResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.delay):
	}
	return s.base.Exec(ctx, cmd)
}
func (s *slowStubChannel) Upload(_ context.Context, _ string, _ io.Reader) error { return nil }
func (s *slowStubChannel) Download(_ context.Context, _ string) (io.Reader, error) {
	return nil, nil
}
func (s *slowStubChannel) IsConnected() bool { return true }

// Compile-time interface check.
var _ channel.Channel = (*stubGateChannel)(nil)
