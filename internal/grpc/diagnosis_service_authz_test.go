package grpc

// diagnosis_service_authz_test.go covers the gate that Decide's own fixture
// makes sharp: Diagnose executes commands on the requested host with the
// platform's credentials, so it is judged as `apply` on THAT HOST'S
// environment — not on whatever default the deployment happens to carry.
//
// The staging host is the load-bearing case: alice holds plan+view in dev and
// only view in staging, so any path that forgets to consult the resolver (or
// resolves to the default env) lets her diagnose a staging host. Every
// refusal also asserts the report cache stayed empty, and the allowed case
// asserts it did NOT — otherwise "nothing was cached" would be true of a
// service that never ran anything at all.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/nexus/levee/internal/diagnosis"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

func seededDiagService(t *testing.T, withResolver bool) (*DiagnosisService, state.Store) {
	t.Helper()
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, row := range []*state.Target{
		{ID: "tgt-dev-1", Hostname: "dev-1.example", ChannelType: "ssh", Port: 22, Status: "active", Labels: map[string]string{"env": "dev"}, CreatedAt: now},
		{ID: "tgt-stg-1", Hostname: "stg-1.example", ChannelType: "ssh", Port: 22, Status: "active", Labels: map[string]string{"env": "staging"}, CreatedAt: now},
		{ID: "tgt-nolabel", Hostname: "bare.example", ChannelType: "ssh", Port: 22, Status: "active", CreatedAt: now},
	} {
		require.NoError(t, store.UpsertTarget(ctx, row))
	}

	svc := NewDiagnosisService(diagnosis.NewDiagEngine(diagnosis.DiagEngineConfig{}), nil).
		WithAuthorizer(newServiceAuthorizer(t))
	if withResolver {
		svc.WithTargetEnvResolver(TargetEnvResolver(store))
	}
	return svc, store
}

func cachedReportCount(svc *DiagnosisService) int {
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	return len(svc.reports)
}

func TestDiagnose_RefusedForEnvTheSubjectHasNoApplyFor(t *testing.T) {
	svc, _ := seededDiagService(t, true)

	// alice: plan+view in dev, view only in staging. Staging is the whole
	// point — a gate that resolved nothing would judge her in dev and pass.
	_, err := svc.Diagnose(asSubject("alice"), &pb.DiagnoseRequest{Target: "stg-1.example"})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, codeOf(t, err), "the refusal must be a policy denial, not a wiring error: %v", err)
	assert.Contains(t, err.Error(), "staging", "the refusal must name the environment it judged")
	assert.Equal(t, 0, cachedReportCount(svc), "a refused diagnosis must not leave a cached report")
}

func TestDiagnose_AllowedForAdminInDevAndCachesReport(t *testing.T) {
	svc, _ := seededDiagService(t, true)

	// dave: admin in dev, and admin is the superset that covers apply.
	rep, err := svc.Diagnose(asSubject("dave"), &pb.DiagnoseRequest{Target: "dev-1.example"})
	require.NoError(t, err, "the allowed path must work, or the refusal assertions prove nothing")
	require.NotNil(t, rep)
	assert.Equal(t, 1, cachedReportCount(svc), "the accepted diagnosis is cached under its report id")
}

func TestDiagnose_RefusesTargetTheInventoryDoesNotKnow(t *testing.T) {
	svc, _ := seededDiagService(t, true)

	_, err := svc.Diagnose(asSubject("dave"), &pb.DiagnoseRequest{Target: "10.9.9.9"})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, codeOf(t, err))
	assert.Contains(t, err.Error(), "does not know this host",
		"an unattributable target is refused because there is no environment to license it against")
	assert.Equal(t, 0, cachedReportCount(svc))
}

// TestDiagnose_KnownTargetWithoutEnvLabelUsesTheDefault: bare.example is
// registered but unlabeled. That is the configured meaning of
// permission.default_env (dev here), which dave administers — so the gate must
// allow it. It is a different case from an unknown host, and collapsing them
// would either lock operators out of their own default env or start trusting
// random addresses.
func TestDiagnose_KnownTargetWithoutEnvLabelUsesTheDefault(t *testing.T) {
	svc, _ := seededDiagService(t, true)

	_, err := svc.Diagnose(asSubject("dave"), &pb.DiagnoseRequest{Target: "bare.example"})
	require.NoError(t, err, "an unlabeled but registered host resolves to the default env (dev), where dave is admin")
}

func TestDiagnose_FailsLoudlyWhenResolverIsMissing(t *testing.T) {
	svc, _ := seededDiagService(t, false) // authorizer installed, resolver not

	_, err := svc.Diagnose(asSubject("dave"), &pb.DiagnoseRequest{Target: "dev-1.example"})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err),
		"a half-installed gate must be named as a wiring bug, not quietly judged against the default env")
	assert.Contains(t, err.Error(), "resolver")
}

func TestDiagnose_NoAuthorizerMeansNoGate(t *testing.T) {
	store := newTestStore(t)
	svc := NewDiagnosisService(diagnosis.NewDiagEngine(diagnosis.DiagEngineConfig{}), nil).
		WithTargetEnvResolver(TargetEnvResolver(store))

	rep, err := svc.Diagnose(asSubject("whoever"), &pb.DiagnoseRequest{Target: "anywhere"})
	require.NoError(t, err, "a deployment with no policy answers Unimplemented-free and ungated, like every other service")
	require.NotNil(t, rep)
}

func TestGetDiagnosis_EnforcedPerReportEnv(t *testing.T) {
	svc, _ := seededDiagService(t, true)

	// A dev report comes from the real path; the staging one is seeded into the
	// cache because NO subject in the fixture may diagnose staging — which is
	// exactly what makes the read gate worth testing separately: reading is
	// judged per report, not by whoever was allowed to create it.
	dev, err := svc.Diagnose(asSubject("dave"), &pb.DiagnoseRequest{Target: "dev-1.example"})
	require.NoError(t, err)
	svc.cacheReport(diagnosis.DiagnosticReport{ID: "rep-stg", Target: "stg-1.example"})

	// alice holds view in dev AND view in staging, so both are readable.
	_, err = svc.GetDiagnosis(asSubject("alice"), &pb.GetDiagnosisRequest{Id: dev.GetId()})
	require.NoError(t, err)
	_, err = svc.GetDiagnosis(asSubject("alice"), &pb.GetDiagnosisRequest{Id: "rep-stg"})
	require.NoError(t, err, "alice may read a staging report: she holds view there")

	// dave is admin in dev and holds nothing in staging. If the read gate
	// resolved nothing, or judged the default env, he would read rep-stg too.
	_, err = svc.GetDiagnosis(asSubject("dave"), &pb.GetDiagnosisRequest{Id: "rep-stg"})
	require.Error(t, err, "the read gate must judge the report's own environment, not the default")
	assert.Equal(t, codes.PermissionDenied, codeOf(t, err))
	assert.Contains(t, err.Error(), "staging")

	// carol holds approve only: not even a read — while dave still reads his own
	// dev report, which is what makes her refusal a policy answer rather than a
	// broken RPC.
	_, err = svc.GetDiagnosis(asSubject("carol"), &pb.GetDiagnosisRequest{Id: dev.GetId()})
	assert.Equal(t, codes.PermissionDenied, codeOf(t, err))
	_, err = svc.GetDiagnosis(asSubject("dave"), &pb.GetDiagnosisRequest{Id: dev.GetId()})
	require.NoError(t, err)

	// An unregistered subject is refused as well, after the id resolves — the
	// trade-off stated on the gate.
	_, err = svc.GetDiagnosis(asSubject("mallory"), &pb.GetDiagnosisRequest{Id: dev.GetId()})
	assert.Equal(t, codes.PermissionDenied, codeOf(t, err))
}
