package grpc

// rest_exposure_matrix_test.go pins the RPC→REST exposure matrix at RPC
// granularity.
//
// Why this exists: TestGatewayRouterTablesCoverTheDocumentedServices pins the
// service NAMES and the RESTful resource FAMILIES, and that was not enough. An
// entire service — InventoryService, six RPCs, policy-gated, unit-tested — sat
// fully registered on the gRPC server while BOTH route trees ignored it: not in
// the proto-JSON table, no RESTful resource. Every CLI command worked in local
// mode (direct store access), so the gap stayed invisible until a hand audit
// walked proto → gRPC registration → REST routes → frontend wrappers. The
// ledger recorded it as "服务完整、网关静默".
//
// What this guard adds: for every RPC in the binary's own proto descriptors, it
// asks WHERE the gateway exposes it, and fails when the answer is nowhere.
//
// The exposure model has two legitimate shapes:
//   - proto-JSON: the service is in grpcServiceHandlers and its handler
//     switches on the method name (handleAlert's "ReceiveAlert"/...);
//   - RESTful: the resource family is in restResourceHandlers and the dispatch
//     tree routes the path shapes the frontend calls.
//
// An allowlist carries the RPCs that are deliberately NOT exposed over REST,
// each with the reason. Adding an RPC and forgetting the gateway turns this
// red with a message naming the RPC and the two legitimate answers.

import (
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rpcExemptFromREST carries the RPCs deliberately absent from the REST surface,
// each with the reason. It is an allowlist, not a count: counts let one entry
// leave while another arrives.
//
//   - gRPC-only streams (the gateway's proto-JSON table answers "streaming
//     RPCs not supported" for them, and the SSE routes carry the same data
//     where the console needs it);
//   - agent registry writes (Register/Heartbeat/Deregister are the AGENT's
//     protocol, spoken over gRPC with metadata auth — the operator-facing
//     reads are exposed, the writes are not a browser's business);
//   - internal ingress (ReceiveAlert is a machine-to-machine feed; the
//     credential headers live on the gRPC path).
var rpcExemptFromREST = map[string]string{
	"ChangeService.WatchChange":                 "gRPC server stream; the SSE route /changes/{id}/events carries the same fan-out",
	"ChangeService.StreamLogs":                  "gRPC server stream; log tailing is served by the SSE routes and GetLogs paging",
	"AlertService.SubscribeAlerts":              "gRPC server stream; the gateway answers 501 for it (see handleAlert) — SSE route is the ledger's open item",
	"AlertService.ReceiveAlert":                 "machine-to-machine ingress; the per-source credential headers are defined on the gRPC path",
	"AgentService.RegisterAgent":                "agent-protocol write: an agent speaks gRPC with its credential, not a browser",
	"AgentService.AgentHeartbeat":               "agent-protocol write: same trust boundary as RegisterAgent",
	"AgentService.DeregisterAgent":              "agent-protocol write: same trust boundary as RegisterAgent",
	"SystemService.RunDoctor":                   "CLI/diagnostic surface; the console reads /system/status and /system/doctor's REST twin exists in the operational tree",
	"ConversationService.SubscribeConversation": "gRPC server stream; conversation polling is via the session REST endpoints",
}

// restMatrixProtoPkg is the proto package the served API declares its services
// in. It mirrors cmd/levee's protoPkg (package-private there); renaming the
// proto package turns both red together.
const restMatrixProtoPkg = "levee"

// protoRPCsInBinary enumerates every RPC the binary's proto descriptors carry:
// {service, method}. Sourced from protoregistry — the same "not from a list
// this file could forget" discipline the policy census uses.
func protoRPCsInBinary(t *testing.T) [][2]string {
	t.Helper()
	var out [][2]string
	protoregistry.GlobalFiles.RangeFilesByPackage(protoreflect.FullName(restMatrixProtoPkg), func(fd protoreflect.FileDescriptor) bool {
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			svc := svcs.Get(i)
			svcName := string(svc.FullName())
			for m := 0; m < svc.Methods().Len(); m++ {
				out = append(out, [2]string{svcName, string(svc.Methods().Get(m).Name())})
			}
		}
		return true
	})
	require.NotEmpty(t, out, "no RPCs found in proto package %q — the matrix would compare against nothing", restMatrixProtoPkg)
	return out
}

// TestEveryRPCHasADocumentedRESTAnswer fails when an RPC is reachable through
// NEITHER proto-JSON NOR a RESTful resource family.
//
// The proto-JSON check reads the gateway's own table (not a copy), so renaming
// a service in the table turns this red in the same run. The RESTful check
// reads restResourceHandlers and asserts the FAMILY the RPC's service maps to
// is present — the per-RPC path shapes are pinned by the frontend contract
// test, which walks web/src/api and would catch a dropped path.
func TestEveryRPCHasADocumentedRESTAnswer(t *testing.T) {
	gw := &Gateway{}
	handlers := gw.grpcServiceHandlers()
	resources := gw.restResourceHandlers()

	// service → the RESTful resource families that carry its RPCs. A service
	// counts as exposed if it is in the proto-JSON table OR any of its
	// families is routed; the mapping is the REAL one (e.g. InventoryService's
	// RPCs live under the "groups" family and the targets tree), so a service
	// whose families all vanish turns red even though the service name itself
	// is still in a table somewhere.
	restFamiliesByService := map[string][]string{
		"ChangeService":       {"changes"},
		"TemplateService":     {"templates"},
		"TargetService":       {"targets"},
		"AuditService":        {"audit"},
		"SystemService":       {"system"},
		"AlertService":        {}, // proto-JSON only; stream is 501 (exempt below)
		"DiagnosisService":    {}, // proto-JSON only
		"ConversationService": {"conversation"},
		"InventoryService":    {"groups", "targets"}, // groups tree + import/status/history off targets
		"AgentService":        {"agents"},            // reads + RemoveAgent; Register/Heartbeat/Deregister stay gRPC-only
	}

	var unrouted []string
	for _, rpc := range protoRPCsInBinary(t) {
		svc, method := rpc[0], rpc[1]
		short := svc[strings.LastIndex(svc, ".")+1:]

		_, protoJSONRouted := handlers[short]
		restFamilyRouted := false
		for _, family := range restFamiliesByService[short] {
			if _, ok := resources[family]; ok {
				restFamilyRouted = true
				break
			}
		}

		if protoJSONRouted || restFamilyRouted {
			continue
		}
		reason, exempt := rpcExemptFromREST[short+"."+method]
		if !exempt {
			unrouted = append(unrouted, short+"."+method)
			continue
		}
		// An exempt RPC must not secretly become routed: the reason would go
		// stale, and a stale exemption is how an exception table stops meaning
		// anything (the same rule the policy census applies to its own table).
		assert.False(t, protoJSONRouted && restFamilyRouted,
			"%s.%s is exempt from REST (%s) but both route shapes now exist — delete the exemption with its reason",
			short, method, reason)
	}
	assert.Empty(t, unrouted,
		"these RPCs are reachable through NEITHER the proto-JSON table NOR a RESTful resource family; "+
			"expose them or add them to rpcExemptFromREST with a reason: %v", unrouted)
}

// TestExemptRPCsStillExist fails when an entry in rpcExemptFromREST names an
// RPC the binary no longer carries — a stale exemption is dead weight that
// reads like a decision.
func TestExemptRPCsStillExist(t *testing.T) {
	carried := map[string]bool{}
	for _, rpc := range protoRPCsInBinary(t) {
		short := rpc[0][strings.LastIndex(rpc[0], ".")+1:]
		carried[short+"."+rpc[1]] = true
	}
	var stale []string
	for name := range rpcExemptFromREST {
		if !carried[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	assert.Empty(t, stale,
		"rpcExemptFromREST names RPCs the binary does not carry (deleted or renamed?): %v", stale)
}
