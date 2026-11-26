package grpc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

const createChangeTemplateWorkflow = `name: create-from-template
target:
  type: host
  query: "env=test"
steps:
  - name: restart-app
    action: svc.restart
    args:
      name: "{{.service}}"
`

func TestCreateChangeTemplateNameStoresRenderedWorkflowSource(t *testing.T) {
	svc, store := newTestChangeService(t)
	templates := NewTemplateService(store, nil)
	_, err := templates.CreateTemplate(context.Background(), &pb.CreateTemplateRequest{
		Name: "restart-service", WorkflowContent: createChangeTemplateWorkflow, RequiredParams: []string{"service"},
	})
	require.NoError(t, err)
	svc.WithTemplateService(templates)

	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{
		Label: "restart-nginx", TemplateName: "restart-service", Params: map[string]string{"service": "nginx"},
	})
	require.NoError(t, err)
	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Contains(t, run.WorkflowName, `name: "nginx"`)
	assert.NotContains(t, run.WorkflowName, "{{.service}}")
	assert.Equal(t, "restart-service", run.TemplateName)
}

func TestCreateChangeTemplateNameWithoutResolverKeepsLegacyDraft(t *testing.T) {
	svc, store := newTestChangeService(t)
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{TemplateName: "metadata-only"})
	require.NoError(t, err, "reduced embedders keep their legacy metadata-only draft contract")

	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Empty(t, run.WorkflowName)
	assert.Equal(t, "metadata-only", run.TemplateName)
}

func TestCreateChangeMissingTemplateFailsBeforeWrite(t *testing.T) {
	svc, store := newTestChangeService(t)
	svc.WithTemplateService(NewTemplateService(store, nil))
	_, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{TemplateName: "not-there"})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))

	runs, listErr := store.ListRuns(context.Background(), state.RunFilter{})
	require.NoError(t, listErr)
	assert.Empty(t, runs)
}

func TestCreateChangeExplicitWorkflowFileKeepsPrecedenceWithoutResolver(t *testing.T) {
	svc, store := newTestChangeService(t)
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{
		TemplateName: "metadata-only", WorkflowFile: "workflow.levee",
	})
	require.NoError(t, err)
	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Equal(t, "workflow.levee", run.WorkflowName)
	assert.Equal(t, "metadata-only", run.TemplateName)
}
