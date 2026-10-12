package grpc

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/authz"
	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
)

// rest_gate_test.go pins the read gates on the two REST-only endpoints that read
// the store directly and therefore never reached a service's authorizer:
// GET /system/cluster-status and GET /system/batch-status.
//
// Before this, both answered every authenticated caller, including a subject
// whose team had no grant at all. /system/status, /system/config and
// /system/doctor were never in that state: they delegate to SystemService,
// which gates them itself (config at admin, doctor at view, status narrows by
// visible environment). An earlier note in the roadmap said "four endpoints";
// checking each handler showed it was two, and this file is why it stays two.

// authorizerWith writes a users/matrix pair into a temp dir and loads it.
func authorizerWith(t *testing.T, users, matrix, defaultEnv string) *authz.Authorizer {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "users.yaml"), []byte(users), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, authz.MatrixFileName), []byte(matrix), 0o600))
	a, err := authz.Load(dir, defaultEnv)
	require.NoError(t, err)
	require.True(t, a.Enforced())
	return a
}

func TestRESTGate_ClusterStatusRequiresRead(t *testing.T) {
	// A named token for alice: without one every request carries no subject, and
	// an unattributable caller is admitted by design (filtering it would deny
	// everyone holding the same token). The gate can only be observed for a
	// request that PRESENTS an identity.
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{
		AuthTokens: []TokenIdentity{{Token: "alice-secret", Subject: "alice"}},
	})
	gw.SetStore(store)

	// No matrix configured: nothing to judge against, so the endpoint admits.
	// That is the posture every other gate has (and serve announces it at
	// startup rather than deciding per request).
	noPolicy := authedGet(t, srv.URL+"/system/cluster-status")
	defer func() { _ = noPolicy.Body.Close() }()
	require.Equal(t, http.StatusOK, noPolicy.StatusCode)

	// A matrix that grants the subject nothing in the deployment's default
	// environment: refused. The node registry is deployment-wide — it declares
	// no environment — so it is judged in default_env.
	gw.WithAuthorizer(authorizerWith(t,
		"users:\n  - name: alice\n    team: sre\n    role: operator\n",
		"teams:\n  - name: sre\n    environments:\n      - name: staging\n        actions: [view]\n",
		"prod"))
	denied := authedGet(t, srv.URL+"/system/cluster-status")
	defer func() { _ = denied.Body.Close() }()
	require.Equal(t, http.StatusForbidden, denied.StatusCode,
		"an unauthorised subject must not read the node registry")

	// The same subject with a grant in the default environment: admitted.
	gw.WithAuthorizer(authorizerWith(t,
		"users:\n  - name: alice\n    team: sre\n    role: operator\n",
		"teams:\n  - name: sre\n    environments:\n      - name: prod\n        actions: [view]\n",
		"prod"))
	granted := authedGet(t, srv.URL+"/system/cluster-status")
	defer func() { _ = granted.Body.Close() }()
	require.Equal(t, http.StatusOK, granted.StatusCode)
}

func TestRESTGate_BatchStatusIsJudgedOnTheRunsOwnEnvironment(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{
		AuthTokens: []TokenIdentity{{Token: "alice-secret", Subject: "alice"}},
	})
	gw.SetStore(store)
	ctx := context.Background()
	now := time.Now().UTC()

	// Two runs in two environments. The subject is granted staging only, so the
	// caller's default environment is deliberately NOT the one the readable run
	// lives in: a gate that judged default_env instead of the run's env would
	// pass here by accident.
	for _, id := range []string{"run-prod", "run-staging"} {
		env := "prod"
		if id == "run-staging" {
			env = "staging"
		}
		require.NoError(t, store.CreateRun(ctx, &state.Run{
			ID: id, WorkflowName: "wf", Status: runstatus.StatusRunning,
			Creator: "liwg", CreatedAt: now, UpdatedAt: now, IncidentID: env,
		}))
	}

	gw.WithAuthorizer(authorizerWith(t,
		"users:\n  - name: alice\n    team: sre\n    role: operator\n",
		"teams:\n  - name: sre\n    environments:\n      - name: staging\n        actions: [view]\n",
		"prod"))

	allowed := authedGet(t, srv.URL+"/system/batch-status?run_id=run-staging")
	defer func() { _ = allowed.Body.Close() }()
	require.Equal(t, http.StatusOK, allowed.StatusCode)

	denied := authedGet(t, srv.URL+"/system/batch-status?run_id=run-prod")
	defer func() { _ = denied.Body.Close() }()
	require.Equal(t, http.StatusForbidden, denied.StatusCode,
		"a run in an unreadable environment must not leak its progress")

	// An unknown id is 404 BEFORE the gate: an id typo is not a policy question,
	// and answering 403 would send the operator hunting a permission problem
	// that does not exist (the same ordering the SSE route uses).
	missing := authedGet(t, srv.URL+"/system/batch-status?run_id=run-nope")
	defer func() { _ = missing.Body.Close() }()
	require.Equal(t, http.StatusNotFound, missing.StatusCode)
}

// authedGet issues a GET presenting alice's named token, so the request carries
// a subject the policy can judge.
func authedGet(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer alice-secret")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}
