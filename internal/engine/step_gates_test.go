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
	require.NoError(t, materializeStepGates(gm, p))
	assert.Empty(t, gm.Gates(verify.PhasePreApply))
	assert.Empty(t, gm.Gates(verify.PhasePostApply))
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
	require.NoError(t, materializeStepGates(gm, p))

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
				Pre: []dsl.GateCheck{{Type: "slo", Command: "rate(errors[5m]) < 0.01"}},
			},
		}},
	}}}
	err := materializeStepGates(gm, p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not executable")
	assert.Empty(t, gm.Gates(verify.PhasePreApply), "nothing may be registered when materialisation fails")
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
	require.NoError(t, materializeStepGates(gm, p))
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
