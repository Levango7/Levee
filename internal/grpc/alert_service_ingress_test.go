// alert_service_ingress_test.go covers the two layers that closed the alert
// exemption: the per-source ingress credential (a machine identity, so the code
// is Unauthenticated rather than a matrix refusal) and `view` over the alert's
// OWNED environment, which is resolved from the registered source and never
// from the message.
package grpc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"github.com/nexus/levee/internal/alert"
	"github.com/nexus/levee/internal/grpc/pb"
)

func alertSourcesForTest(t *testing.T) *alert.SourceRegistry {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, alert.SourcesFileName), []byte(`
sources:
  - name: prom-dev
    environment: dev
    secret: dev-secret
  - name: prom-prod
    environment: prod
    secret: prod-secret
`), 0o600))
	reg, err := alert.LoadSources(dir)
	require.NoError(t, err)
	require.True(t, reg.Configured())
	return reg
}

// alertIngressCtx is the shape a real caller uses: the source name and token
// arrive as transport metadata (gRPC metadata; the REST gateway bridges its two
// headers into the same keys).
func alertIngressCtx(ctx context.Context, source, token string) context.Context {
	return metadata.NewIncomingContext(ctx, metadata.New(map[string]string{
		alert.SourceMetadataKey: source,
		alert.TokenMetadataKey:  token,
	}))
}

func alertMsg(id string) *pb.AlertMessage {
	return &pb.AlertMessage{Id: id, Source: "prom", Title: "cpu high"}
}

func TestAlertService_IngressRequiresARegisteredSource(t *testing.T) {
	svc := NewAlertService(nil, nil).WithAlertSources(alertSourcesForTest(t))

	// No credential at all.
	_, err := svc.ReceiveAlert(context.Background(), alertMsg("al-none"))
	assert.Equal(t, codes.Unauthenticated, codeOf(t, err))

	// Unregistered source.
	_, err = svc.ReceiveAlert(alertIngressCtx(context.Background(), "ghost-monitor", "whatever"), alertMsg("al-ghost"))
	assert.Equal(t, codes.Unauthenticated, codeOf(t, err))

	// Registered source, wrong secret.
	_, err = svc.ReceiveAlert(alertIngressCtx(context.Background(), "prom-dev", "not-the-secret"), alertMsg("al-bad"))
	assert.Equal(t, codes.Unauthenticated, codeOf(t, err))

	// A refused alert must leave no trace in the ring.
	assert.Nil(t, svc.lookupAlert("al-ghost"))
	assert.Nil(t, svc.lookupAlert("al-bad"))

	// Correct credential: accepted and attributed to the source's environment.
	resp, err := svc.ReceiveAlert(alertIngressCtx(context.Background(), "prom-dev", "dev-secret"), alertMsg("al-ok"))
	require.NoError(t, err)
	assert.Equal(t, "accepted", resp.GetStatus())
	a := svc.lookupAlert("al-ok")
	require.NotNil(t, a)
	assert.Equal(t, "dev", a.Environment, "the environment comes from the registration, never the payload")
}

func TestAlertService_ReadIsJudgedOnTheAlertsOwnedEnvironment(t *testing.T) {
	svc := NewAlertService(nil, nil).
		WithAlertSources(alertSourcesForTest(t)).
		WithAuthorizer(newServiceAuthorizer(t))

	// A dev alert: dave (admin in dev) sees it, carol (approve in dev but no
	// view) does not.
	_, err := svc.ReceiveAlert(alertIngressCtx(context.Background(), "prom-dev", "dev-secret"), alertMsg("al-dev"))
	require.NoError(t, err)
	_, err = svc.GetAlertStatus(asSubject("dave"), &pb.GetAlertStatusRequest{Id: "al-dev"})
	require.NoError(t, err, "the allowed path must work or the refusal assertions prove nothing")
	_, err = svc.GetAlertStatus(asSubject("carol"), &pb.GetAlertStatusRequest{Id: "al-dev"})
	requireDenied(t, err, "GetAlertStatus (dev alert, no view)")

	// A prod alert: no fixture team reaches prod, so even the dev admin cannot
	// read it. The judgement is the alert's environment, not the caller's
	// default one.
	_, err = svc.ReceiveAlert(alertIngressCtx(context.Background(), "prom-prod", "prod-secret"), alertMsg("al-prod"))
	require.NoError(t, err)
	_, err = svc.GetAlertStatus(asSubject("dave"), &pb.GetAlertStatusRequest{Id: "al-prod"})
	requireDenied(t, err, "GetAlertStatus (prod alert)")
}

func TestAlertService_NoSourcesKeepsAuthenticationOnlyIngress(t *testing.T) {
	// No alert-sources.yaml: the pre-existing posture. The alert is accepted
	// without a per-source credential and is explicitly unattributed, which the
	// read gate then resolves through default_env rather than guessing an owner.
	svc := NewAlertService(nil, nil)

	resp, err := svc.ReceiveAlert(context.Background(), alertMsg("al-1"))
	require.NoError(t, err)
	assert.Equal(t, "accepted", resp.GetStatus())
	a := svc.lookupAlert("al-1")
	require.NotNil(t, a)
	assert.Empty(t, a.Environment)
}
