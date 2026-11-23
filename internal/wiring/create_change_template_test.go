package wiring

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	leveegrpc "github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
)

func TestCreateChangeTemplateNameCanGeneratePlan(t *testing.T) {
	e, store := newTestEngine(t, "web-1")
	templates := leveegrpc.NewTemplateService(store, nil)
	_, err := templates.CreateTemplate(context.Background(), &pb.CreateTemplateRequest{
		Name: "restart-service", WorkflowContent: createChangeTemplateWorkflowForPlan, RequiredParams: []string{"service"},
	})
	require.NoError(t, err)
	changes := leveegrpc.NewChangeService(store, nil, nil, nil).WithTemplateService(templates)
	created, err := changes.CreateChange(context.Background(), &pb.CreateChangeRequest{
		Label: "restart-nginx", TemplateName: "restart-service", Params: map[string]string{"service": "nginx"},
	})
	require.NoError(t, err)

	msg, stored, err := e.GeneratePlan(context.Background(), created.GetId(), []string{"web-1"})
	require.NoError(t, err, "template-created changes must reach plan instead of failing for missing workflow source")
	require.NotNil(t, stored)
	assert.Equal(t, created.GetId(), msg.GetChangeId())
	assert.NotEmpty(t, stored.Hash)
}

const createChangeTemplateWorkflowForPlan = `name: create-from-template
target:
  type: host
  query: "env=test"
steps:
  - name: restart-app
    action: svc.restart
    args:
      name: "{{.service}}"
`
