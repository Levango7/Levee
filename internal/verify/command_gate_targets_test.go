package verify

// command_gate_targets_test.go — 按目标执行命令门禁的判定规则。
//
// 钉住四件事：① 每台都要过，一台失败就是整批失败（"web-1 过了"不等于"这批过了"）；
// ② 连不上的目标算失败而不是跳过（没被证明健康的机器不能算检查通过）；
// ③ 失败结论要指名是哪台，且所有 offender 都在，不能因为第一台失败就看不见第二台；
// ④ 没有目标就是失败关闭，绝不因为"没有东西要查"而判通过。
// 另外守住向后兼容：provider 缺席时仍走原来的单通道 / missing channel 路径。

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
)

// hostProvider hands out one scripted channel per host. A host with no entry is
// unreachable, which is exactly the case that must count as a failure.
type hostProvider struct {
	mu     sync.Mutex
	chans  map[string]*fakeChannel
	dialed []string
}

func (p *hostProvider) provider() GateChannelProvider {
	return func(_ context.Context, host string) (channel.Channel, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.dialed = append(p.dialed, host)
		if ch, ok := p.chans[host]; ok {
			return ch, nil
		}
		return nil, fmt.Errorf("no route to %s", host)
	}
}

func (p *hostProvider) snapshotDialed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.dialed...)
}

func TestCommandGate_AllTargetsPassVerifiesThePhase(t *testing.T) {
	probe := &hostProvider{chans: map[string]*fakeChannel{
		"web-1": newFakeChannel(execResult(0, "active")),
		"web-2": newFakeChannel(execResult(0, "active")),
	}}
	g := NewCommandGate("svc", PhasePostBatch, "systemctl is-active nginx")

	res, err := g.Check(context.Background(), GateInput{
		RunID: "run-1", BatchID: "b-0", TargetIDs: []string{"web-1", "web-2"},
		ChannelFor: probe.provider(),
	})
	require.NoError(t, err)
	assert.True(t, res.Passed, "every target passed, so the phase passed: %s", res.Message)
	assert.ElementsMatch(t, []string{"web-1", "web-2"}, probe.snapshotDialed(),
		"the check must reach every target, not just the first")
	for _, host := range []string{"web-1", "web-2"} {
		assert.Equal(t, []string{"systemctl is-active nginx"}, probe.chans[host].commands,
			"%s must have been asked to run the declared command", host)
	}
}

func TestCommandGate_OneTargetFailingFailsTheBatch(t *testing.T) {
	probe := &hostProvider{chans: map[string]*fakeChannel{
		"web-1": newFakeChannel(execResult(0, "active")),
		"web-2": newFakeChannel(execResult(3, "inactive")),
	}}
	g := NewCommandGate("svc", PhasePostBatch, "systemctl is-active nginx")

	res, err := g.Check(context.Background(), GateInput{
		RunID: "run-2", BatchID: "b-0", TargetIDs: []string{"web-1", "web-2"},
		ChannelFor: probe.provider(),
	})
	require.NoError(t, err)
	assert.False(t, res.Passed, "one failing target means the batch is not verified")
	assert.Contains(t, res.Message, "web-2", "the verdict must name the offending host")
	assert.Equal(t, []string{"web-2"}, res.Details["failed_targets"])
	assert.Contains(t, res.Message, "1 of 2", "the count tells the operator the blast radius")
}

func TestCommandGate_UnreachableTargetCountsAsFailure(t *testing.T) {
	probe := &hostProvider{chans: map[string]*fakeChannel{
		"web-1": newFakeChannel(execResult(0, "active")),
	}}
	g := NewCommandGate("svc", PhasePostBatch, "systemctl is-active nginx")

	res, err := g.Check(context.Background(), GateInput{
		RunID: "run-3", BatchID: "b-0", TargetIDs: []string{"web-1", "web-down"},
		ChannelFor: probe.provider(),
	})
	require.NoError(t, err)
	assert.False(t, res.Passed, "a host we could not reach has not been shown to be healthy")
	assert.Contains(t, res.Message, "web-down")
	targets, ok := res.Details["targets"].(map[string]any)
	require.True(t, ok, "per-target evidence must be preserved for the audit trail")
	entry, ok := targets["web-down"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, entry["error"], "no route", "the reason must be the dial failure itself")
}

func TestCommandGate_NoTargetsFailsClosed(t *testing.T) {
	probe := &hostProvider{chans: map[string]*fakeChannel{}}
	g := NewCommandGate("svc", PhasePostBatch, "systemctl is-active nginx")

	res, err := g.Check(context.Background(), GateInput{
		RunID: "run-4", TargetIDs: nil, ChannelFor: probe.provider(),
	})
	require.NoError(t, err)
	assert.False(t, res.Passed, "an empty target set is not evidence of health")
	assert.Equal(t, "no_targets", res.Details["reason"])
	assert.Empty(t, probe.snapshotDialed(), "nothing may be dialled for a check with no targets")
}

func TestCommandGate_WithoutProviderKeepsTheSingleChannelPath(t *testing.T) {
	// Backward-compat guard: a caller that hands over one channel (an ad-hoc
	// /gates/verify request, a unit test) must see exactly the old behaviour —
	// per-target fan-out is an addition, not a replacement.
	g := NewCommandGate("svc", PhasePostApply, "systemctl is-active nginx")
	ch := newFakeChannel(execResult(0, "active"))

	res, err := g.Check(context.Background(), GateInput{RunID: "run-5", Channel: ch})
	require.NoError(t, err)
	assert.True(t, res.Passed)
	assert.EqualValues(t, 1, ch.calls.Load())
}
