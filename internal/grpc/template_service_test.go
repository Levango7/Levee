// template_service_test.go tests the TemplateService gRPC handler. It uses
// a real SQLite store and the in-memory template registry (no file-backed
// library) so tests are hermetic.
package grpc

import (
	"context"
	"testing"

	"github.com/nexus/levee/internal/grpc/pb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// newTestTemplateService returns a TemplateService backed by a fresh test
// store and an in-memory template registry (lib=nil).
func newTestTemplateService(t *testing.T) *TemplateService {
	t.Helper()
	store := newTestStore(t)
	return NewTemplateService(store, nil)
}

// =========================================================================
// CreateTemplate
// =========================================================================

func TestCreateTemplate_Success(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	resp, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		Name:            "deploy-web",
		Description:     "Deploy web app",
		WorkflowContent: "name: deploy\nsteps: []",
		RequiredParams:  []string{"version"},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, "deploy-web", resp.GetName())
	assert.Equal(t, "Deploy web app", resp.GetDescription())
	assert.Equal(t, "name: deploy\nsteps: []", resp.GetWorkflowContent())
	assert.Equal(t, []string{"version"}, resp.GetRequiredParams())
	assert.True(t, resp.GetCreatedAt() > 0)
	assert.True(t, resp.GetUpdatedAt() > 0)
}

func TestCreateTemplate_Duplicate(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	req := &pb.CreateTemplateRequest{
		Name:            "tmpl-1",
		WorkflowContent: "steps: []",
	}
	_, err := svc.CreateTemplate(ctx, req)
	require.NoError(t, err)

	_, err = svc.CreateTemplate(ctx, req)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.AlreadyExists, st.Code())
}

func TestCreateTemplate_Overwrite(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	_, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		Name:            "tmpl-1",
		WorkflowContent: "v1",
	})
	require.NoError(t, err)

	resp, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		Name:            "tmpl-1",
		WorkflowContent: "v2",
		Overwrite:       true,
	})
	require.NoError(t, err)
	assert.Equal(t, "v2", resp.GetWorkflowContent())
}

func TestCreateTemplate_MissingName(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	_, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		WorkflowContent: "steps: []",
	})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}

func TestCreateTemplate_MissingContent(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	_, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		Name: "tmpl-1",
	})
	require.Error(t, err)
}

func TestCreateTemplate_NilRequest(t *testing.T) {
	svc := newTestTemplateService(t)
	_, err := svc.CreateTemplate(context.Background(), nil)
	require.Error(t, err)
}

// =========================================================================
// GetTemplate
// =========================================================================

func TestGetTemplate_Success(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	_, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		Name:            "tmpl-1",
		WorkflowContent: "steps: []",
	})
	require.NoError(t, err)

	resp, err := svc.GetTemplate(ctx, &pb.GetTemplateRequest{Name: "tmpl-1"})
	require.NoError(t, err)
	assert.Equal(t, "tmpl-1", resp.GetName())
}

func TestGetTemplate_NotFound(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	_, err := svc.GetTemplate(ctx, &pb.GetTemplateRequest{Name: "nope"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
}

func TestGetTemplate_EmptyName(t *testing.T) {
	svc := newTestTemplateService(t)
	_, err := svc.GetTemplate(context.Background(), &pb.GetTemplateRequest{Name: ""})
	require.Error(t, err)
}

// =========================================================================
// ListTemplates
// =========================================================================

func TestListTemplates_Empty(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	resp, err := svc.ListTemplates(ctx, &pb.ListTemplatesRequest{})
	require.NoError(t, err)
	assert.Empty(t, resp.GetTemplates())
	assert.Equal(t, int32(0), resp.GetTotalSize())
}

func TestListTemplates_All(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	for _, name := range []string{"alpha", "beta", "gamma"} {
		_, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
			Name: name, WorkflowContent: "steps: []",
		})
		require.NoError(t, err)
	}

	resp, err := svc.ListTemplates(ctx, &pb.ListTemplatesRequest{})
	require.NoError(t, err)
	assert.Equal(t, int32(3), resp.GetTotalSize())
	// Should be sorted by name.
	assert.Equal(t, "alpha", resp.GetTemplates()[0].GetName())
	assert.Equal(t, "beta", resp.GetTemplates()[1].GetName())
	assert.Equal(t, "gamma", resp.GetTemplates()[2].GetName())
}

func TestListTemplates_NameFilter(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	for _, name := range []string{"deploy-web", "deploy-db", "rollback-web"} {
		_, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
			Name: name, WorkflowContent: "steps: []",
		})
		require.NoError(t, err)
	}

	resp, err := svc.ListTemplates(ctx, &pb.ListTemplatesRequest{NameContains: "deploy"})
	require.NoError(t, err)
	assert.Equal(t, int32(2), resp.GetTotalSize())
}

func TestListTemplates_Pagination(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		name := string(rune('a'+i)) + "-tmpl"
		_, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
			Name: name, WorkflowContent: "steps: []",
		})
		require.NoError(t, err)
	}

	resp, err := svc.ListTemplates(ctx, &pb.ListTemplatesRequest{PageSize: 2})
	require.NoError(t, err)
	assert.Len(t, resp.GetTemplates(), 2)
	assert.Equal(t, int32(5), resp.GetTotalSize())
	assert.NotEmpty(t, resp.GetNextPageToken())
}

// =========================================================================
// DeleteTemplate
// =========================================================================

func TestDeleteTemplate_Success(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	_, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		Name: "tmpl-1", WorkflowContent: "steps: []",
	})
	require.NoError(t, err)

	_, err = svc.DeleteTemplate(ctx, &pb.DeleteTemplateRequest{Name: "tmpl-1"})
	require.NoError(t, err)

	// Verify gone.
	_, err = svc.GetTemplate(ctx, &pb.GetTemplateRequest{Name: "tmpl-1"})
	require.Error(t, err)
}

func TestDeleteTemplate_NotFound(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	_, err := svc.DeleteTemplate(ctx, &pb.DeleteTemplateRequest{Name: "nope"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
}

func TestDeleteTemplate_EmptyName(t *testing.T) {
	svc := newTestTemplateService(t)
	_, err := svc.DeleteTemplate(context.Background(), &pb.DeleteTemplateRequest{Name: ""})
	require.Error(t, err)
}

// =========================================================================
// InstantiateTemplate
// =========================================================================

func TestInstantiateTemplate_DryRun(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	_, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		Name:            "deploy",
		WorkflowContent: "version: {{.version}}",
		RequiredParams:  []string{"version"},
	})
	require.NoError(t, err)

	resp, err := svc.InstantiateTemplate(ctx, &pb.InstantiateTemplateRequest{
		TemplateName: "deploy",
		Label:        "deploy-v1",
		Params:       map[string]string{"version": "1.2.3"},
		DryRun:       true,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.NotEmpty(t, resp.GetId())
	assert.Equal(t, "planned", resp.GetStatus())
	assert.Equal(t, "deploy", resp.GetTemplateName())
	assert.Equal(t, "1.2.3", resp.GetParams()["version"])
}

// Normal (non-dry-run) instantiation creates runs with status "pending",
// matching the shared lifecycle vocabulary — the old "pending_approval"
// value was unknown to the rest of the state machine.
func TestInstantiateTemplate_NormalRunPending(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	_, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		Name:            "deploy",
		WorkflowContent: "steps: []",
	})
	require.NoError(t, err)

	resp, err := svc.InstantiateTemplate(ctx, &pb.InstantiateTemplateRequest{
		TemplateName: "deploy",
		Label:        "deploy-v2",
	})
	require.NoError(t, err)
	assert.Equal(t, "pending", resp.GetStatus())
}

func TestInstantiateTemplate_MissingRequiredParam(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	_, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		Name:            "deploy",
		WorkflowContent: "version: {{.version}}",
		RequiredParams:  []string{"version"},
	})
	require.NoError(t, err)

	_, err = svc.InstantiateTemplate(ctx, &pb.InstantiateTemplateRequest{
		TemplateName: "deploy",
		DryRun:       true,
	})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}

func TestInstantiateTemplate_TemplateNotFound(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	_, err := svc.InstantiateTemplate(ctx, &pb.InstantiateTemplateRequest{
		TemplateName: "nope",
		DryRun:       true,
	})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
}

func TestInstantiateTemplate_EmptyTemplateName(t *testing.T) {
	svc := newTestTemplateService(t)
	_, err := svc.InstantiateTemplate(context.Background(), &pb.InstantiateTemplateRequest{})
	require.Error(t, err)
}

// Ensure emptypb is used.
var _ emptypb.Empty
