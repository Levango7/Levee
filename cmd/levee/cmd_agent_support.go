package main

import (
	"sync"

	"github.com/nexus/levee/internal/agent"
)

// globalAgentRegistry is the process-wide registry used by the CLI's
// `agent list / show / remove` commands. In remote mode the CLI would
// instead query the master over gRPC; in local mode (the MVP default)
// the CLI shares the registry with the in-process master client.
var (
	globalAgentRegistry     *agent.AgentRegistry
	globalAgentRegistryOnce sync.Once
)

// getGlobalAgentRegistry returns the process-wide agent registry,
// initialising it on first use.
func getGlobalAgentRegistry() *agent.AgentRegistry {
	globalAgentRegistryOnce.Do(func() {
		globalAgentRegistry = agent.NewAgentRegistry()
	})
	return globalAgentRegistry
}

// globalMasterClient is the process-wide in-process master client. It
// is created on first use and shares the global registry so that
// `agent start` and `agent list` see the same agents.
var (
	globalMasterClient     *agent.InProcessMasterClient
	globalMasterClientOnce sync.Once
)

// getGlobalMasterClient returns the process-wide in-process master
// client, initialising it on first use.
func getGlobalMasterClient() *agent.InProcessMasterClient {
	globalMasterClientOnce.Do(func() {
		globalMasterClient = agent.NewInProcessMasterClient(getGlobalAgentRegistry())
	})
	return globalMasterClient
}

// newInProcessMasterClient returns the process-wide in-process master
// client for the given master address. The address is currently
// ignored (the in-process client does not dial out); it is accepted so
// that the CLI flag plumbing stays consistent with the future gRPC
// client.
func newInProcessMasterClient(masterAddr string) *agent.InProcessMasterClient {
	_ = masterAddr
	return getGlobalMasterClient()
}
