// rest_gate_authz_test.go pins the authorization gate on POST /gates/verify.
//
// A cmd check dials a real host and runs the caller's command with the
// platform's stored credentials. Until this gate existed the endpoint
// authenticated the caller and then asked nothing else: a subject holding
// any valid token — including one scoped to dev view only — could run an
// arbitrary shell command on any registered host, prod included, which is
// precisely the surface resource_authz.go opens by arguing every other
// write goes through policy. The gate is judged on the target's own
// declared environment (labels["env"], same source as every other
// target-scoped decision), and it runs BEFORE the dial so an unauthorized
// host is never even contacted.

package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/authz"
	"github.com/nexus/levee/internal/state"
)

// gateAuthorizer grants the sre team apply in dev and view in staging —
// the split that lets one host carry the allow case and one carry the deny.
func gateAuthorizer(t *testing.T) *authz.Authorizer {
	t.Helper()
	return authorizerWith(t,
		"users:\n  - name: sre\n    team: sre\n    role: operator\n",
		"teams:\n  - name: sre\n    environments:\n      - name: dev\n        actions: [apply, view]\n      - name: staging\n        actions: [view]\n",
		"dev")
}

func gateTarget(t *testing.T, store state.Store, id, host, env string) {
	t.Helper()
	require.NoError(t, store.UpsertTarget(context.Background(), &state.Target{
		ID:        id,
		Hostname:  host,
		Port:      22,
		Status:    "active",
		Labels:    map[string]string{"env": env},
		CreatedAt: time.Now().UTC(),
	}))
}

func cmdVerifyRequest(target string) *GateVerifyRequest {
	return &GateVerifyRequest{
		Type:   "cmd",
		Target: target,
		Params: map[string]any{"cmd": "echo hi", "expect_exit": 0},
	}
}

func TestGateVerify_CmdDeniedWithoutApplyOnTargetEnv(t *testing.T) {
	store := newTestStore(t)
	gateTarget(t, store, "t-stg", "stg-host", "staging")

	svc := NewGateService(store, nil, "").WithAuthorizer(gateAuthorizer(t))
	_, err := svc.Verify(asSubject("sre"), cmdVerifyRequest("stg-host"))

	require.Error(t, err, "a view-only subject must not run commands on a staging host")
	require.Equal(t, codes.PermissionDenied, status.Code(err),
		"the refusal must be a permission denial, not a dial or runtime failure: %v", err)
}

func TestGateVerify_CmdAllowedWithApplyOnTargetEnv(t *testing.T) {
	store := newTestStore(t)
	gateTarget(t, store, "t-dev", "dev-host", "dev")

	// No dialer is wired, so an authorized caller gets past the gate and
	// fails closed at the transport with the explicit no-dialer error.
	// That error is how we know the gate itself was passed.
	svc := NewGateService(store, nil, "").WithAuthorizer(gateAuthorizer(t))
	_, err := svc.Verify(asSubject("sre"), cmdVerifyRequest("dev-host"))

	require.Error(t, err)
	require.NotEqual(t, codes.PermissionDenied, status.Code(err),
		"an apply holder must not be refused by the policy: %v", err)
	require.Contains(t, err.Error(), "channel dialer",
		"an authorized caller should reach the dial, which then fails closed over the missing transport")
}

func TestGateVerify_UnknownTargetRefused(t *testing.T) {
	store := newTestStore(t)

	svc := NewGateService(store, nil, "").WithAuthorizer(gateAuthorizer(t))
	_, err := svc.Verify(asSubject("sre"), cmdVerifyRequest("ghost-host"))

	require.Error(t, err)
	require.Contains(t, err.Error(), "not registered",
		"an unregistered host must be refused at the gate, not reached by a dial attempt")
}

func TestGateVerify_NoPolicyKeepsDeploymentDefault(t *testing.T) {
	store := newTestStore(t)
	gateTarget(t, store, "t-np", "np-host", "dev")

	// Nil authorizer is the deployment-wide no-policy posture every other
	// service documents; it must stay a gate-level short-circuit, not a
	// per-endpoint fail-open that only this service could regress into.
	svc := NewGateService(store, nil, "")
	_, err := svc.Verify(asSubject("sre"), cmdVerifyRequest("np-host"))

	require.Error(t, err)
	require.Contains(t, err.Error(), "channel dialer",
		"with no policy configured the gate admits and the dial fails closed")
}
