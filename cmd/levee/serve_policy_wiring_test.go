// serve_policy_wiring_test.go proves the property the service-level tests
// cannot: that the process `levee serve` actually starts installs the policy
// authorizer into every service it registers — and that the services it does
// NOT police are a decision somebody wrote down.
//
// A gate that exists in a service struct is not a gate. Before this file, no
// test in cmd/levee referenced the authorizer at all — `changeSvc.WithAuthorizer`
// was a line in one function, and the fleet services were constructed in two
// different places (buildServeServices and the registration site), so nothing
// failed if either stopped passing it. What is asserted here is the observable
// consequence of the wiring: a subject the matrix does not grant is refused by
// the service the running process would use, and an unconfigured deployment is
// not.
//
// The list of services is not hand-carried any more. It is enumerated from the
// compiled proto descriptors (servedProtoServices), so a service added to
// proto/levee*.proto has to be classified here before the suite goes green:
// either it gets a policed probe, or it appears in the exemption table with a
// reason. That is the failure mode this batch exists to close — alert,
// diagnosis and conversation were served for months while the census said
// "seven services", and nothing went red.
//
// The exemption table is asserted too, not just trusted: an entry needs a
// non-empty reason AND at least one call to probe, every exempt call must
// answer an ungranted subject and a granted subject identically (that is what
// "not policed" means as an observable), and the two sets must be disjoint and
// together equal the census. An exemption list that could grow silently would
// be the same decoration the wire-contract `forbid` list was before it was
// enforced.
package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/nexus/levee/internal/alert"
	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// The matrix grants the sre team everything EXCEPT admin in dev, and platform
// admin — so "denied" here can only come from the policy being consulted, and
// "allowed for dave but not alice" can only come from the matrix itself.
const wiringMatrixYAML = `
teams:
  - name: sre
    environments:
      - name: dev
        actions: [plan, apply, approve, rollback, view]
  - name: platform
    environments:
      - name: dev
        actions: [admin]
`

const wiringRolesYAML = `
roles:
  - name: operator
    permissions: [view]
  - name: steward
    permissions: [view]
`

const wiringUsersYAML = `
users:
  - name: alice
    team: sre
    role: operator
  - name: dave
    team: platform
    role: steward
`

// alertSourcesYAML registers two monitors, one per environment. The credential
// proves WHICH source is speaking; the registration decides which environment
// its alerts belong to. The census probes use the dev source (readable by the
// granted subject) and the prod source (readable by neither fixture team, so
// the refusal can only be the policy).
const alertSourcesYAML = `
sources:
  - name: prom-dev
    environment: dev
    secret: secret-dev
  - name: prom-prod
    environment: prod
    secret: secret-prod
`

const (
	wiringAlertDevSource  = "prom-dev"
	wiringAlertDevSecret  = "secret-dev"
	wiringAlertProdSource = "prom-prod"
	wiringAlertProdSecret = "secret-prod"
)

// protoPkg is the package the served API declares its services in; the census
// is every service in it, whether or not this file remembers to classify them.
const protoPkg = "levee"

const (
	svcAlert        = protoPkg + ".AlertService"
	svcAgent        = protoPkg + ".AgentService"
	svcAudit        = protoPkg + ".AuditService"
	svcChange       = protoPkg + ".ChangeService"
	svcConversation = protoPkg + ".ConversationService"
	svcDiagnosis    = protoPkg + ".DiagnosisService"
	svcInventory    = protoPkg + ".InventoryService"
	svcSystem       = protoPkg + ".SystemService"
	svcTarget       = protoPkg + ".TargetService"
	svcTemplate     = protoPkg + ".TemplateService"
)

// newWiringServices builds the services exactly the way `levee serve` does,
// over a throwaway data directory and store, and seeds the rows the probes
// judge: a dev run, a prod run, and a dev- labelled host.
func newWiringServices(t *testing.T, withMatrix bool) (serveServices, state.Store) {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	if withMatrix {
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "permissions.yaml"), []byte(wiringMatrixYAML), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "roles.yaml"), []byte(wiringRolesYAML), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "users.yaml"), []byte(wiringUsersYAML), 0o600))
	}
	// The alert-source registry is ingress configuration, not policy, so it is
	// written in both fixtures: the gate it installs is independent of whether a
	// permission matrix exists.
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "alert-sources.yaml"), []byte(alertSourcesYAML), 0o600))

	store, err := state.NewSQLiteStore(context.Background(), filepath.Join(dir, "levee.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	cfg := &config.Config{}
	cfg.Server.DataDir = dataDir
	cfg.Permission.DefaultEnv = "dev"

	svcs, err := buildServeServices(context.Background(), store, cfg, nil)
	require.NoError(t, err, "buildServeServices must work on a zero config")
	seedWiringData(t, store)
	return svcs, store
}

func seedWiringData(t *testing.T, store state.Store) {
	t.Helper()
	ctx := context.Background()
	// A dev run so the change and audit probes have something the granted
	// subject IS entitled to; a prod run so a gate that judged the wrong
	// environment would still be caught by the fleet-write test below.
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "run-dev", WorkflowName: "name: dev\n", Status: "approved", IncidentID: "dev",
	}))
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "run-prod", WorkflowName: "name: prod\n", Status: "approved", IncidentID: "prod",
	}))
	require.NoError(t, store.UpsertTarget(ctx, &state.Target{
		ID: "tgt-dev-1", Hostname: "dev-1.example", ChannelType: "ssh", Port: 22,
		Status: "active", Labels: map[string]string{"env": "dev"}, CreatedAt: time.Now().UTC(),
	}))
}

func deniedAs(t *testing.T, want codes.Code, err error, rpc string) {
	t.Helper()
	require.Error(t, err, "%s reached the service without judgement", rpc)
	st, ok := status.FromError(err)
	require.True(t, ok, "%s: %v", rpc, err)
	assert.Equal(t, want, st.Code(), "%s: %v", rpc, err)
}

// postAlert calls ReceiveAlert as a registered source. That is now the only way
// an alert enters the ring: the credential authenticates the MACHINE, and the
// alert is attributed to that source's registered environment.
func postAlert(ctx context.Context, svc *grpc.AlertService, source, secret, id string) error {
	ctx = metadata.NewIncomingContext(ctx, metadata.New(map[string]string{
		alert.SourceMetadataKey: source,
		alert.TokenMetadataKey:  secret,
	}))
	_, err := svc.ReceiveAlert(ctx, &pb.AlertMessage{Id: id, Source: "prom", Title: "cpu high"})
	return err
}

// servedProtoServices enumerates the universe the census has to cover, from the
// descriptors the binary carries — not from a list this file could forget.
func servedProtoServices(t *testing.T) []string {
	t.Helper()
	var names []string
	protoregistry.GlobalFiles.RangeFilesByPackage(protoreflect.FullName(protoPkg), func(fd protoreflect.FileDescriptor) bool {
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			names = append(names, string(svcs.Get(i).FullName()))
		}
		return true
	})
	sort.Strings(names)
	// A census that came back empty would make every assertion below vacuously
	// true, which is the same silence this rewrite exists to remove.
	require.NotEmpty(t, names, "no services found in proto package %q — the census would compare against nothing", protoPkg)
	return names
}

// policedCall is one service's share of the wiring proof: a call an ungranted
// subject must be refused on, the action the refusal must name, and the same
// call for a subject the matrix does grant.
type policedCall struct {
	deny  func(ctx context.Context) error
	allow func(ctx context.Context) error
	via   string
}

// exemptCall is a service the policy deliberately does not judge, with the
// reason and the calls that prove the exemption is still the truth.
type exemptCall struct {
	reason string
	calls  map[string]func(ctx context.Context) error
}

func policedWiringCalls(svcs serveServices) map[string]policedCall {
	return map[string]policedCall{
		svcChange: {
			deny: func(ctx context.Context) error {
				_, err := svcs.changeSvc.ApplyChange(ctx, &pb.ApplyChangeRequest{ChangeId: "run-dev"})
				return err
			},
			allow: func(ctx context.Context) error {
				_, err := svcs.changeSvc.ApplyChange(ctx, &pb.ApplyChangeRequest{ChangeId: "run-dev"})
				return err
			},
			via: "apply",
		},
		svcTemplate: {
			deny: func(ctx context.Context) error {
				_, err := svcs.templateSvc.CreateTemplate(ctx, &pb.CreateTemplateRequest{Name: "t1", WorkflowContent: "x"})
				return err
			},
			allow: func(ctx context.Context) error {
				_, err := svcs.templateSvc.CreateTemplate(ctx, &pb.CreateTemplateRequest{Name: "t2", WorkflowContent: "x"})
				return err
			},
			via: "admin",
		},
		svcTarget: {
			deny: func(ctx context.Context) error {
				_, err := svcs.targetSvc.AddTarget(ctx, &pb.AddTargetRequest{Id: "t-mallory", Hostname: "h1.example"})
				return err
			},
			allow: func(ctx context.Context) error {
				_, err := svcs.targetSvc.AddTarget(ctx, &pb.AddTargetRequest{Id: "t-dave", Hostname: "h2.example"})
				return err
			},
			via: "admin",
		},
		svcAudit: {
			deny: func(ctx context.Context) error {
				_, err := svcs.auditSvc.GetRunReport(ctx, &pb.GetRunReportRequest{RunId: "run-dev"})
				return err
			},
			allow: func(ctx context.Context) error {
				_, err := svcs.auditSvc.GetRunReport(ctx, &pb.GetRunReportRequest{RunId: "run-dev"})
				return err
			},
			via: "view",
		},
		svcSystem: {
			deny: func(ctx context.Context) error {
				_, err := svcs.systemSvc.GetConfig(ctx, &pb.GetConfigRequest{})
				return err
			},
			allow: func(ctx context.Context) error {
				_, err := svcs.systemSvc.GetConfig(ctx, &pb.GetConfigRequest{})
				return err
			},
			via: "admin",
		},
		svcInventory: {
			deny: func(ctx context.Context) error {
				_, err := svcs.inventorySvc.CreateGroup(ctx, &pb.CreateGroupRequest{Name: "g-mallory"})
				return err
			},
			allow: func(ctx context.Context) error {
				_, err := svcs.inventorySvc.CreateGroup(ctx, &pb.CreateGroupRequest{Name: "g-dave"})
				return err
			},
			via: "admin",
		},
		svcAgent: {
			deny: func(ctx context.Context) error {
				_, err := svcs.agentSvc.RemoveAgent(ctx, &pb.RemoveAgentRequest{Id: "ghost"})
				return err
			},
			allow: func(ctx context.Context) error {
				_, err := svcs.agentSvc.RemoveAgent(ctx, &pb.RemoveAgentRequest{Id: "ghost"})
				return err
			},
			via: "admin",
		},
		// Diagnosis is the one probe whose refusal can only come from the
		// resolver AND the matrix both being wired: dev-1.example is registered
		// with env=dev, so "unknown host" and "default env" are both ruled out
		// as explanations for what comes back.
		svcDiagnosis: {
			deny: func(ctx context.Context) error {
				_, err := svcs.diagSvc.Diagnose(ctx, &pb.DiagnoseRequest{Target: "dev-1.example"})
				return err
			},
			allow: func(ctx context.Context) error {
				_, err := svcs.diagSvc.Diagnose(ctx, &pb.DiagnoseRequest{Target: "dev-1.example"})
				return err
			},
			via: "apply",
		},
		svcConversation: {
			deny: func(ctx context.Context) error {
				_, err := svcs.convSvc.SendMessage(ctx, &pb.SendMessageRequest{UserId: "whoever", Text: "hi"})
				return err
			},
			allow: func(ctx context.Context) error {
				_, err := svcs.convSvc.SendMessage(ctx, &pb.SendMessageRequest{UserId: "whoever", Text: "hi"})
				return err
			},
			via: "plan",
		},
		// AlertService's gate is two-layered and only the second layer is the
		// matrix: ingress is a per-source credential (a machine, not a team), and
		// the READ side is `view` over the alert's owned environment. The census
		// probes the read layer because that is where a subject is judged; the
		// ingress layer has its own test below.
		svcAlert: {
			deny: func(ctx context.Context) error {
				if err := postAlert(ctx, svcs.alertSvc, wiringAlertProdSource, wiringAlertProdSecret, "al-prod"); err != nil {
					return err
				}
				_, err := svcs.alertSvc.GetAlertStatus(ctx, &pb.GetAlertStatusRequest{Id: "al-prod"})
				return err
			},
			allow: func(ctx context.Context) error {
				if err := postAlert(ctx, svcs.alertSvc, wiringAlertDevSource, wiringAlertDevSecret, "al-dev"); err != nil {
					return err
				}
				_, err := svcs.alertSvc.GetAlertStatus(ctx, &pb.GetAlertStatusRequest{Id: "al-dev"})
				return err
			},
			via: "view",
		},
	}
}

// exemptWiringCalls is the table of services the policy deliberately does not
// judge. It is empty on purpose and kept as the MECHANISM, not as content: the
// census requires every served service to be policed or exempt-with-a-reason,
// so a future exemption has to be declared here with a reason and a probe that
// shows it still holds. AlertService was the last entry — it is policed now
// (ingress identity + `view` on the alert's owned environment), so the table
// carries nothing.
func exemptWiringCalls(_ serveServices) map[string]exemptCall {
	return map[string]exemptCall{}
}

// TestServePolicyCensusClassifiesEveryServedService is the enumeration guard:
// the census comes from the proto descriptors, and every service in it must be
// either policed (with a working probe) or exempt with a reason and a probe
// that keeps the exemption honest.
func TestServePolicyCensusClassifiesEveryServedService(t *testing.T) {
	svcs, _ := newWiringServices(t, true)
	census := servedProtoServices(t)
	policed := policedWiringCalls(svcs)
	exempt := exemptWiringCalls(svcs)

	var classified []string
	for name := range policed {
		classified = append(classified, name)
	}
	for name := range exempt {
		require.NotContains(t, policed, name, "%s cannot be policed and exempt at once", name)
		classified = append(classified, name)
	}
	sort.Strings(classified)
	assert.Equal(t, census, classified,
		"every service the API declares must be policed or exempt-with-a-reason: %v vs %v", census, classified)

	// The exemption table is not allowed to be a decorative list: each entry
	// states why and names at least one call that shows the exemption is real.
	// Its size is pinned as a ratchet, so a new exemption is always a decision
	// somebody made in this file rather than a line appended somewhere else.
	// It is zero today: AlertService, the last exemption, is policed now.
	require.Len(t, exempt, 0, "no service is exempt today; adding one must be a deliberate edit here, with a reason and a probe")
	for name, e := range exempt {
		assert.GreaterOrEqual(t, len(strings.TrimSpace(e.reason)), 40,
			"%s is exempt without a reason an operator could read", name)
		assert.NotEmpty(t, e.calls, "%s is exempt but nothing probes that the exemption still holds", name)
		for _, fn := range e.calls {
			require.NotNil(t, fn)
		}
	}

	// Neither side may be a stub: every policed service needs both halves, and
	// the refusal must name the action it judged.
	for name, p := range policed {
		require.NotNil(t, p.deny, "%s has no ungranted-subject probe", name)
		require.NotNil(t, p.allow, "%s has no granted-subject probe, so its refusal could be a broken service", name)
		assert.NotEmpty(t, p.via, "%s must name the action the refusal is expected to quote", name)
	}

	// The policed set must actually be policed BY THE MATRIX, which needs the
	// fixture to be enforced; without this the whole file could be green on a
	// deployment that simply has no policy.
	require.True(t, svcs.authzSvc.Enforced(), "the fixture must produce an enforced matrix")
}

// TestServePolicyRefusesEveryUngrantedCall is the wiring proof: mallory is a
// named caller the registry does not know, and each policed service refuses her
// through the code path a real request takes. PermissionDenied rather than
// Unauthenticated because she IS named — it is simply not in users.yaml. The
// anonymous case is pinned in internal/grpc/service_policy_test.go.
func TestServePolicyRefusesEveryUngrantedCall(t *testing.T) {
	svcs, _ := newWiringServices(t, true)
	ctx := grpc.ContextWithSubject(context.Background(), "mallory")

	calls := policedWiringCalls(svcs)
	for _, name := range sortedNames(calls) {
		p := calls[name]
		err := p.deny(ctx)
		deniedAs(t, codes.PermissionDenied, err, name)
		assert.Contains(t, err.Error(), p.via, "%s: the refusal must name the action it judged", name)
		assert.Contains(t, err.Error(), "mallory", "%s: the refusal must name the subject it judged", name)
	}
}

// TestServePolicyAdmitsEveryGrantedCall is the positive control that keeps the
// refusals above from being a broken service: dave's team holds admin in dev,
// and no call in the census comes back as a policy answer for him. The other
// status codes are this file's business only insofar as they are NOT a refusal.
func TestServePolicyAdmitsEveryGrantedCall(t *testing.T) {
	svcs, _ := newWiringServices(t, true)
	ctx := grpc.ContextWithSubject(context.Background(), "dave")

	calls := policedWiringCalls(svcs)
	for _, name := range sortedNames(calls) {
		err := calls[name].allow(ctx)
		if err == nil {
			continue
		}
		code := status.Code(err)
		assert.NotEqual(t, codes.PermissionDenied, code, "%s: dave holds admin in dev, so this is a policy answer where there should be none: %v", name, err)
		assert.NotEqual(t, codes.Unauthenticated, code, "%s: a named subject must never land on Unauthenticated: %v", name, err)
	}
}

func sortedNames[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestServeExemptCallsAnswerBothSubjectsAlike is the exemption table's own
// guard: "not policed" is an observable claim, so an exempt service must answer
// an ungranted subject and a granted subject identically, and neither answer may
// be a policy refusal. The table is empty today, so this loop asserts nothing —
// it is kept because it is what makes a FUTURE exemption honest: adding one
// without a probe that still holds turns this red.
func TestServeExemptCallsAnswerBothSubjectsAlike(t *testing.T) {
	svcs, _ := newWiringServices(t, true)
	mallory := grpc.ContextWithSubject(context.Background(), "mallory")
	dave := grpc.ContextWithSubject(context.Background(), "dave")

	exempt := exemptWiringCalls(svcs)
	for _, name := range sortedNames(exempt) {
		for _, cn := range sortedNames(exempt[name].calls) {
			fn := exempt[name].calls[cn]
			errUngranted := fn(mallory)
			errGranted := fn(dave)
			assert.Equal(t, status.Code(errUngranted), status.Code(errGranted),
				"%s.%s answered the two subjects differently, so it is policed and the census is stale", name, cn)
			assert.NotEqual(t, codes.PermissionDenied, status.Code(errUngranted), "%s.%s: %v", name, cn, errUngranted)
			assert.NotEqual(t, codes.Unauthenticated, status.Code(errUngranted), "%s.%s: %v", name, cn, errUngranted)
		}
	}
}

// TestServeAlertIngressRequiresARegisteredSource is the ingress half of the
// alert gate: with alert-sources.yaml present, an alert without a registered
// source credential is refused (Unauthenticated), a registered one is accepted
// and attributed to that source's environment, and the read gate then judges
// the caller against it.
func TestServeAlertIngressRequiresARegisteredSource(t *testing.T) {
	svcs, _ := newWiringServices(t, true)
	dave := grpc.ContextWithSubject(context.Background(), "dave")

	// No credential: refused before the alert reaches the ring.
	_, err := svcs.alertSvc.ReceiveAlert(dave, &pb.AlertMessage{Id: "al-nocred", Source: "prom", Title: "cpu high"})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))

	// Wrong secret: refused.
	err = postAlert(dave, svcs.alertSvc, wiringAlertDevSource, "wrong-secret", "al-badsecret")
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))

	// Unregistered source: refused.
	err = postAlert(dave, svcs.alertSvc, "ghost-monitor", "whatever", "al-ghost")
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))

	// Registered source + correct secret: accepted, and readable by a subject
	// entitled to its environment.
	require.NoError(t, postAlert(dave, svcs.alertSvc, wiringAlertDevSource, wiringAlertDevSecret, "al-ok"))
	statusResp, err := svcs.alertSvc.GetAlertStatus(dave, &pb.GetAlertStatusRequest{Id: "al-ok"})
	require.NoError(t, err, "dave holds admin in dev and must see a dev alert: %v", err)
	assert.NotEmpty(t, statusResp.GetStatus())

	// The same alert is not visible to an ungranted subject.
	_, err = svcs.alertSvc.GetAlertStatus(grpc.ContextWithSubject(context.Background(), "mallory"),
		&pb.GetAlertStatusRequest{Id: "al-ok"})
	deniedAs(t, codes.PermissionDenied, err, "GetAlertStatus (ungranted)")
}

// TestServePolicyDeniesFleetWritesThatChangeGrantsAllow is the half that makes
// the census more than "unknown subject is refused": alice is a real,
// registered team member whose matrix entry covers every change action, yet
// the fleet writes still stop at her — because `admin` is what they ask for.
func TestServePolicyDeniesFleetWritesThatChangeGrantsAllow(t *testing.T) {
	svcs, _ := newWiringServices(t, true)
	alice := grpc.ContextWithSubject(context.Background(), "alice")

	_, err := svcs.changeSvc.CreateChange(alice, &pb.CreateChangeRequest{Label: "ok", Environment: "dev"})
	require.NoError(t, err, "alice may create a change in dev: %v", err)

	_, err = svcs.targetSvc.AddTarget(alice, &pb.AddTargetRequest{Id: "t1", Hostname: "h1"})
	deniedAs(t, codes.PermissionDenied, err, "AddTarget (no admin grant)")
	_, err = svcs.templateSvc.CreateTemplate(alice, &pb.CreateTemplateRequest{Name: "t", WorkflowContent: "x"})
	deniedAs(t, codes.PermissionDenied, err, "CreateTemplate (no admin grant)")
	_, err = svcs.systemSvc.GetConfig(alice, &pb.GetConfigRequest{})
	deniedAs(t, codes.PermissionDenied, err, "GetConfig (no admin grant)")
	_, err = svcs.agentSvc.RegisterAgent(alice, &pb.RegisterAgentRequest{Id: "a1", Address: "10.0.0.5:9099"})
	deniedAs(t, codes.PermissionDenied, err, "RegisterAgent (no admin grant)")

	// dave's team holds admin, so the same calls work for him. The refusal
	// above is the matrix speaking, not a hard-coded deny.
	_, err = svcs.targetSvc.AddTarget(grpc.ContextWithSubject(context.Background(), "dave"),
		&pb.AddTargetRequest{Id: "t2", Hostname: "h2"})
	require.NoError(t, err, "dave has admin in dev: %v", err)
	_, err = svcs.agentSvc.RegisterAgent(grpc.ContextWithSubject(context.Background(), "dave"),
		&pb.RegisterAgentRequest{Id: "a2", Address: "10.0.0.6:9099"})
	require.NoError(t, err, "dave may register an agent too: %v", err)
}

// TestServeWithoutMatrixKeepsPreviousBehaviour is the compatibility promise at
// the wiring level: an operator who never wrote permissions.yaml gets the
// binary they had yesterday, for every call in the census — including the ones
// this batch put a gate behind.
func TestServeWithoutMatrixKeepsPreviousBehaviour(t *testing.T) {
	svcs, _ := newWiringServices(t, false)
	require.False(t, svcs.authzSvc.Enforced())

	ctx := grpc.ContextWithSubject(context.Background(), "anyone")
	calls := policedWiringCalls(svcs)
	for _, name := range sortedNames(calls) {
		err := calls[name].deny(ctx)
		if err == nil {
			continue
		}
		code := status.Code(err)
		assert.NotEqual(t, codes.PermissionDenied, code, "%s refused with no matrix configured: %v", name, err)
		assert.NotEqual(t, codes.Unauthenticated, code, "%s demanded an identity with no matrix configured: %v", name, err)
	}
	for name, e := range exemptWiringCalls(svcs) {
		for cn, fn := range e.calls {
			err := fn(ctx)
			assert.NotEqual(t, codes.PermissionDenied, status.Code(err), "%s.%s gained a gate: %v", name, cn, err)
		}
	}
}
