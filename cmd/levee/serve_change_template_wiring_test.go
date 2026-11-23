package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/grpc/pb"
)

func TestServeCreateChangeTemplateNameUsesSharedTemplateService(t *testing.T) {
	svcs, store := newWiringServices(t, false)
	ctx := context.Background()
	const workflow = "name: served-template\nsteps: []\n"
	_, err := svcs.templateSvc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		Name: "served-template", WorkflowContent: workflow,
	})
	require.NoError(t, err)

	created, err := svcs.changeSvc.CreateChange(ctx, &pb.CreateChangeRequest{TemplateName: "served-template"})
	require.NoError(t, err, "the production composition must attach template resolution to CreateChange")
	run, err := store.GetRun(ctx, created.GetId())
	require.NoError(t, err)
	assert.Equal(t, workflow, run.WorkflowName)
}
