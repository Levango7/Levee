// exec_channel_free_test.go pins the dispatcher's channel-free decision: a
// module that declares it owns its transport is handed the inventory row
// without a dialled session, every other module (and every unknown name) keeps
// the connected-channel contract.
package wiring

import (
	"context"
	"testing"

	"github.com/nexus/levee/internal/executor"

	"github.com/stretchr/testify/assert"
)

// channelFreeTestModule implements executor.Module and optionally the
// ChannelFree interface, so one type covers both branches of the decision.
type channelFreeTestModule struct {
	name  string
	needs bool
	off   bool // when true, NeedsChannel is not implemented at all
}

func (m channelFreeTestModule) Name() string      { return m.name }
func (m channelFreeTestModule) Actions() []string { return []string{"run"} }
func (m channelFreeTestModule) Idempotent() bool  { return true }
func (m channelFreeTestModule) Execute(context.Context, string, executor.ModuleInput) (*executor.ModuleOutput, error) {
	return &executor.ModuleOutput{}, nil
}

// plainTestModule deliberately does NOT implement ChannelFree.
type plainTestModule struct{ name string }

func (m plainTestModule) Name() string      { return m.name }
func (m plainTestModule) Actions() []string { return []string{"run"} }
func (m plainTestModule) Idempotent() bool  { return false }
func (m plainTestModule) Execute(context.Context, string, executor.ModuleInput) (*executor.ModuleOutput, error) {
	return &executor.ModuleOutput{}, nil
}

// ownsChannel is the ChannelFree implementation, kept off the shared type so
// plainTestModule can stay interface-free.
type ownsChannel struct{ channelFreeTestModule }

func (m ownsChannel) NeedsChannel() bool { return m.needs }

func TestModuleNeedsChannel(t *testing.T) {
	const (
		freeName  = "test-channel-free"
		plainName = "test-plain"
	)
	executor.DefaultExecutor().RegisterModule(ownsChannel{channelFreeTestModule{name: freeName, needs: false}})
	executor.DefaultExecutor().RegisterModule(plainTestModule{name: plainName})
	defer func() {
		executor.DefaultExecutor().UnregisterModule(freeName)
		executor.DefaultExecutor().UnregisterModule(plainName)
	}()

	assert.False(t, moduleNeedsChannel(freeName),
		"a module declaring NeedsChannel false must not have a session dialled")
	assert.True(t, moduleNeedsChannel(plainName),
		"a module without the optional interface keeps the connected-channel contract")
	assert.True(t, moduleNeedsChannel("no-such-module"),
		"an unknown module keeps the dial so the executor's own error names it")
}
