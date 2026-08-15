package template

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- test helpers -----------------------------------------------------------

// newTestLibrary returns a TemplateLibrary backed by a per-test temp dir.
func newTestLibrary(t *testing.T) *TemplateLibrary {
	t.Helper()
	dir := t.TempDir()
	lib, err := NewTemplateLibrary(dir)
	require.NoError(t, err)
	return lib
}

// sampleTemplate returns a valid Template for testing.
func sampleTemplate(name string) *Template {
	return &Template{
		Name:        name,
		Description: "deploy nginx to target hosts",
		Content:     "name: deploy-nginx\ntarget: {{.target}}\nreplicas: {{.replicas}}\n",
		Parameters: []TemplateParam{
			{Name: "target", Description: "target host list", Required: true, Type: "list"},
			{Name: "replicas", Description: "replica count", Required: false, Default: "3", Type: "int"},
		},
		Tags: map[string]string{
			"team": "sre",
			"env":  "prod",
		},
	}
}

// --- NewTemplateLibrary -----------------------------------------------------

func TestNewTemplateLibrary_CreatesDir(t *testing.T) {
	dir := t.TempDir()
	sub := dir + "/nested/templates"
	lib, err := NewTemplateLibrary(sub)
	require.NoError(t, err)
	require.NotNil(t, lib)
	// Directory should now exist.
	assert.DirExists(t, sub)
}

func TestNewTemplateLibrary_EmptyBaseDir(t *testing.T) {
	_, err := NewTemplateLibrary("")
	require.Error(t, err)
}

// --- Save (create) ----------------------------------------------------------

func TestSave_NewTemplate_PersistsFile(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()
	tmpl := sampleTemplate("deploy-nginx")

	require.NoError(t, lib.Save(ctx, tmpl))

	// ID generated, CreatedAt set, UpdatedAt nil.
	assert.NotEmpty(t, tmpl.ID)
	assert.True(t, strings.HasPrefix(tmpl.ID, "tmpl-"))
	assert.False(t, tmpl.CreatedAt.IsZero())
	assert.Nil(t, tmpl.UpdatedAt)

	// Round-trip via Get.
	got, err := lib.Get(ctx, "deploy-nginx")
	require.NoError(t, err)
	assert.Equal(t, "deploy-nginx", got.Name)
	assert.Equal(t, tmpl.ID, got.ID)
	assert.Equal(t, tmpl.Content, got.Content)
	assert.Len(t, got.Parameters, 2)
	assert.Equal(t, "target", got.Parameters[0].Name)
	assert.True(t, got.Parameters[0].Required)
	assert.Equal(t, "replicas", got.Parameters[1].Name)
	assert.Equal(t, "3", got.Parameters[1].Default)
	assert.Equal(t, "sre", got.Tags["team"])
}

func TestSave_NilTemplate(t *testing.T) {
	lib := newTestLibrary(t)
	err := lib.Save(context.Background(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidTemplate)
}

func TestSave_EmptyName(t *testing.T) {
	lib := newTestLibrary(t)
	tmpl := sampleTemplate("")
	err := lib.Save(context.Background(), tmpl)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyTemplateName)
}

func TestSave_EmptyContent(t *testing.T) {
	lib := newTestLibrary(t)
	tmpl := sampleTemplate("empty-content")
	tmpl.Content = ""
	err := lib.Save(context.Background(), tmpl)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyContent)
}

// --- Save (update via re-Save) ----------------------------------------------

func TestSave_UpdateExisting_PreservesIDAndCreatedAt(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()
	tmpl := sampleTemplate("deploy-nginx")
	require.NoError(t, lib.Save(ctx, tmpl))

	originalID := tmpl.ID
	originalCreatedAt := tmpl.CreatedAt

	// Re-Save with new content.
	tmpl.Content = "name: deploy-nginx\ntarget: {{.target}}\nreplicas: 5\n"
	require.NoError(t, lib.Save(ctx, tmpl))

	assert.Equal(t, originalID, tmpl.ID)
	assert.Equal(t, originalCreatedAt, tmpl.CreatedAt)
	require.NotNil(t, tmpl.UpdatedAt)

	got, err := lib.Get(ctx, "deploy-nginx")
	require.NoError(t, err)
	assert.Equal(t, originalID, got.ID)
	assert.Equal(t, originalCreatedAt, got.CreatedAt)
	require.NotNil(t, got.UpdatedAt)
	assert.Contains(t, got.Content, "replicas: 5")
}

func TestSave_NameConflict_DifferentID(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()
	tmpl := sampleTemplate("deploy-nginx")
	require.NoError(t, lib.Save(ctx, tmpl))

	// Try to save a different template (different ID) with the same name.
	other := sampleTemplate("deploy-nginx")
	other.ID = "tmpl-different-id"
	err := lib.Save(ctx, other)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTemplateExists)
}

// --- Get --------------------------------------------------------------------

func TestGet_Success(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()
	require.NoError(t, lib.Save(ctx, sampleTemplate("deploy-nginx")))

	got, err := lib.Get(ctx, "deploy-nginx")
	require.NoError(t, err)
	assert.Equal(t, "deploy-nginx", got.Name)
}

func TestGet_NotFound(t *testing.T) {
	lib := newTestLibrary(t)
	_, err := lib.Get(context.Background(), "no-such-template")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTemplateNotFound)
}

func TestGet_EmptyName(t *testing.T) {
	lib := newTestLibrary(t)
	_, err := lib.Get(context.Background(), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyTemplateName)
}

// --- List -------------------------------------------------------------------

func TestList_Empty(t *testing.T) {
	lib := newTestLibrary(t)
	got, err := lib.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestList_MultipleTemplates_SortedByName(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()
	require.NoError(t, lib.Save(ctx, sampleTemplate("zebra")))
	require.NoError(t, lib.Save(ctx, sampleTemplate("alpha")))
	require.NoError(t, lib.Save(ctx, sampleTemplate("mid")))

	got, err := lib.List(ctx)
	require.NoError(t, err)
	require.Len(t, got, 3)
	// Sorted ascending.
	assert.Equal(t, "alpha", got[0].Name)
	assert.Equal(t, "mid", got[1].Name)
	assert.Equal(t, "zebra", got[2].Name)
}

func TestList_MultipleTemplates_Independent(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()

	a := sampleTemplate("template-a")
	a.Tags = map[string]string{"k": "a-value"}
	require.NoError(t, lib.Save(ctx, a))

	b := sampleTemplate("template-b")
	b.Tags = map[string]string{"k": "b-value"}
	require.NoError(t, lib.Save(ctx, b))

	got, err := lib.List(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2)

	ga, err := lib.Get(ctx, "template-a")
	require.NoError(t, err)
	gb, err := lib.Get(ctx, "template-b")
	require.NoError(t, err)
	assert.Equal(t, "a-value", ga.Tags["k"])
	assert.Equal(t, "b-value", gb.Tags["k"])
	// IDs must differ.
	assert.NotEqual(t, ga.ID, gb.ID)
}

// --- Delete -----------------------------------------------------------------

func TestDelete_Success(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()
	require.NoError(t, lib.Save(ctx, sampleTemplate("deploy-nginx")))

	require.NoError(t, lib.Delete(ctx, "deploy-nginx"))

	// Subsequent Get fails.
	_, err := lib.Get(ctx, "deploy-nginx")
	assert.ErrorIs(t, err, ErrTemplateNotFound)
}

func TestDelete_NotFound(t *testing.T) {
	lib := newTestLibrary(t)
	err := lib.Delete(context.Background(), "no-such-template")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTemplateNotFound)
}

func TestDelete_EmptyName(t *testing.T) {
	lib := newTestLibrary(t)
	err := lib.Delete(context.Background(), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyTemplateName)
}

// --- Show -------------------------------------------------------------------

func TestShow_Success(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()
	require.NoError(t, lib.Save(ctx, sampleTemplate("deploy-nginx")))

	out, err := lib.Show(ctx, "deploy-nginx")
	require.NoError(t, err)
	assert.Contains(t, out, "Name:        deploy-nginx")
	assert.Contains(t, out, "Description: deploy nginx to target hosts")
	assert.Contains(t, out, "Tags:")
	assert.Contains(t, out, "team: sre")
	assert.Contains(t, out, "env: prod")
	assert.Contains(t, out, "Parameters:")
	assert.Contains(t, out, "- target (list, required)")
	assert.Contains(t, out, "- replicas (int, optional)")
	assert.Contains(t, out, "default=\"3\"")
	assert.Contains(t, out, "Content:")
	assert.Contains(t, out, "target: {{.target}}")
}

func TestShow_NotFound(t *testing.T) {
	lib := newTestLibrary(t)
	_, err := lib.Show(context.Background(), "no-such-template")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTemplateNotFound)
}

func TestShow_NoTagsNoParams(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()
	tmpl := &Template{
		Name:    "bare",
		Content: "name: bare\n",
	}
	require.NoError(t, lib.Save(ctx, tmpl))

	out, err := lib.Show(ctx, "bare")
	require.NoError(t, err)
	assert.Contains(t, out, "Tags:\n  (none)")
	assert.Contains(t, out, "Parameters:\n  (none)")
	assert.Contains(t, out, "Updated:     -")
}

// --- Update -----------------------------------------------------------------

func TestUpdate_AllFields(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()
	require.NoError(t, lib.Save(ctx, sampleTemplate("deploy-nginx")))

	newDesc := "updated description"
	newContent := "name: deploy-nginx-v2\ntarget: {{.target}}\n"
	newParams := []TemplateParam{{Name: "target", Required: true, Type: "string"}}
	newTags := map[string]string{"env": "staging"}

	updated, err := lib.Update(ctx, "deploy-nginx", TemplateUpdate{
		Description: &newDesc,
		Content:     &newContent,
		Parameters:  &newParams,
		Tags:        &newTags,
	})
	require.NoError(t, err)
	assert.Equal(t, newDesc, updated.Description)
	assert.Equal(t, newContent, updated.Content)
	assert.Equal(t, newParams, updated.Parameters)
	assert.Equal(t, newTags, updated.Tags)
	require.NotNil(t, updated.UpdatedAt)

	// Persisted.
	got, err := lib.Get(ctx, "deploy-nginx")
	require.NoError(t, err)
	assert.Equal(t, newDesc, got.Description)
	assert.Equal(t, "staging", got.Tags["env"])
}

func TestUpdate_PartialFields_OnlyProvided(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()
	require.NoError(t, lib.Save(ctx, sampleTemplate("deploy-nginx")))

	// Only update description.
	newDesc := "partial update"
	updated, err := lib.Update(ctx, "deploy-nginx", TemplateUpdate{Description: &newDesc})
	require.NoError(t, err)
	assert.Equal(t, newDesc, updated.Description)
	// Content unchanged.
	assert.Contains(t, updated.Content, "target: {{.target}}")
	// Tags unchanged.
	assert.Equal(t, "sre", updated.Tags["team"])
}

func TestUpdate_EmptyContent(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()
	require.NoError(t, lib.Save(ctx, sampleTemplate("deploy-nginx")))

	empty := ""
	_, err := lib.Update(ctx, "deploy-nginx", TemplateUpdate{Content: &empty})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyContent)
}

func TestUpdate_NotFound(t *testing.T) {
	lib := newTestLibrary(t)
	newDesc := "x"
	_, err := lib.Update(context.Background(), "no-such", TemplateUpdate{Description: &newDesc})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTemplateNotFound)
}

func TestUpdate_EmptyName(t *testing.T) {
	lib := newTestLibrary(t)
	newDesc := "x"
	_, err := lib.Update(context.Background(), "", TemplateUpdate{Description: &newDesc})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyTemplateName)
}

// --- TemplateParam ----------------------------------------------------------

func TestTemplateParam_RequiredDefaultType(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()
	tmpl := &Template{
		Name:    "params-test",
		Content: "name: x\n",
		Parameters: []TemplateParam{
			{Name: "host", Required: true, Type: "string"},
			{Name: "port", Required: false, Default: "8080", Type: "int"},
			{Name: "enabled", Required: false, Default: "true", Type: "bool"},
			{Name: "targets", Required: true, Type: "list"},
		},
	}
	require.NoError(t, lib.Save(ctx, tmpl))

	got, err := lib.Get(ctx, "params-test")
	require.NoError(t, err)
	require.Len(t, got.Parameters, 4)
	assert.True(t, got.Parameters[0].Required)
	assert.Equal(t, "string", got.Parameters[0].Type)
	assert.False(t, got.Parameters[1].Required)
	assert.Equal(t, "8080", got.Parameters[1].Default)
	assert.Equal(t, "int", got.Parameters[1].Type)
	assert.Equal(t, "bool", got.Parameters[2].Type)
	assert.Equal(t, "list", got.Parameters[3].Type)
}

// --- Tags management --------------------------------------------------------

func TestTags_AddModifyRemove(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()

	// Start with no tags.
	tmpl := &Template{Name: "tagged", Content: "name: x\n"}
	require.NoError(t, lib.Save(ctx, tmpl))

	// Add tags via Update.
	tags := map[string]string{"team": "sre", "env": "prod"}
	updated, err := lib.Update(ctx, "tagged", TemplateUpdate{Tags: &tags})
	require.NoError(t, err)
	assert.Equal(t, "sre", updated.Tags["team"])
	assert.Equal(t, "prod", updated.Tags["env"])

	// Modify one tag.
	tags2 := map[string]string{"team": "platform", "env": "prod"}
	updated, err = lib.Update(ctx, "tagged", TemplateUpdate{Tags: &tags2})
	require.NoError(t, err)
	assert.Equal(t, "platform", updated.Tags["team"])

	// Remove a tag (replace with smaller map).
	tags3 := map[string]string{"team": "platform"}
	updated, err = lib.Update(ctx, "tagged", TemplateUpdate{Tags: &tags3})
	require.NoError(t, err)
	assert.Equal(t, "platform", updated.Tags["team"])
	_, hasEnv := updated.Tags["env"]
	assert.False(t, hasEnv)
}

// --- ID stability -----------------------------------------------------------

func TestID_StableAcrossUpdates(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()
	require.NoError(t, lib.Save(ctx, sampleTemplate("stable")))

	got1, err := lib.Get(ctx, "stable")
	require.NoError(t, err)

	newDesc := "desc2"
	_, err = lib.Update(ctx, "stable", TemplateUpdate{Description: &newDesc})
	require.NoError(t, err)

	got2, err := lib.Get(ctx, "stable")
	require.NoError(t, err)

	assert.Equal(t, got1.ID, got2.ID)
	assert.Equal(t, got1.CreatedAt, got2.CreatedAt)
}

// --- Round-trip with all param types ----------------------------------------

func TestSave_AllParamTypes(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()
	tmpl := &Template{
		Name:    "all-types",
		Content: "name: x\n",
		Parameters: []TemplateParam{
			{Name: "s", Type: "string", Required: true},
			{Name: "i", Type: "int", Default: "10"},
			{Name: "b", Type: "bool", Default: "false"},
			{Name: "l", Type: "list", Required: true},
		},
		Tags: map[string]string{"a": "1", "b": "2"},
	}
	require.NoError(t, lib.Save(ctx, tmpl))

	got, err := lib.Get(ctx, "all-types")
	require.NoError(t, err)
	require.Len(t, got.Parameters, 4)
	assert.Equal(t, "string", got.Parameters[0].Type)
	assert.Equal(t, "int", got.Parameters[1].Type)
	assert.Equal(t, "bool", got.Parameters[2].Type)
	assert.Equal(t, "list", got.Parameters[3].Type)
	assert.Equal(t, "1", got.Tags["a"])
}

// --- errors.Is plumbing -----------------------------------------------------

func TestErrors_AreSentinel(t *testing.T) {
	// Ensure the errors are distinct and not wrapping anything.
	assert.NotEqual(t, ErrTemplateNotFound, ErrTemplateExists)
	assert.NotEqual(t, ErrTemplateNotFound, ErrEmptyTemplateName)
	assert.NotEqual(t, ErrTemplateNotFound, ErrEmptyContent)
	assert.NotEqual(t, ErrTemplateNotFound, ErrInvalidTemplate)

	// errors.Is works on the bare error.
	assert.True(t, errors.Is(ErrTemplateNotFound, ErrTemplateNotFound))
}

// --- UpdatedAt semantics ----------------------------------------------------

func TestUpdatedAt_NilOnCreate_SetOnUpdate(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()
	tmpl := sampleTemplate("ts")
	require.NoError(t, lib.Save(ctx, tmpl))
	assert.Nil(t, tmpl.UpdatedAt)

	// On update via re-Save, UpdatedAt is set.
	time.Sleep(10 * time.Millisecond) // ensure timestamp advances
	tmpl.Description = "changed"
	require.NoError(t, lib.Save(ctx, tmpl))
	require.NotNil(t, tmpl.UpdatedAt)
	assert.True(t, tmpl.UpdatedAt.After(tmpl.CreatedAt))
}
