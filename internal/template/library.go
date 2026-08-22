// Template library management for LEVEE.
//
// A Template is a parameterized workflow definition: its Content is a YAML
// workflow document that may contain Go-template-style placeholders (e.g.
// {{.target}}), and its Parameters list describes the placeholders that
// callers must supply when instantiating the template (handled by T062).
//
// The TemplateLibrary stores templates as one JSON file per template under
// a configurable base directory (typically ~/.levee/templates/). The on-disk
// format is a direct JSON encoding of the Template struct, which keeps the
// implementation simple and human-inspectable for the MVP.
//
// Template names are used as the primary lookup key and must be unique within
// a library. Names are validated to be non-empty and are used verbatim as
// file names, so callers should restrict them to filesystem-safe characters.
//
// A TemplateLibrary is safe for concurrent use: all file operations are
// guarded by a sync.RWMutex.
package template

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrTemplateNotFound is returned when a template does not exist.
	ErrTemplateNotFound = errors.New("template: template not found")

	// ErrTemplateExists is returned when Save is called with a name that
	// already belongs to a different template (ID mismatch).
	ErrTemplateExists = errors.New("template: template already exists")

	// ErrEmptyTemplateName is returned when a template name is empty.
	ErrEmptyTemplateName = errors.New("template: empty template name")

	// ErrEmptyContent is returned when a template's content is empty.
	ErrEmptyContent = errors.New("template: empty content")

	// ErrInvalidTemplate is returned when a template fails validation.
	ErrInvalidTemplate = errors.New("template: invalid template")
)

// --- TemplateParam ----------------------------------------------------------

// TemplateParam describes a single placeholder parameter of a template.
// When a template is instantiated (T062), each declared parameter is
// resolved from caller-supplied values, falling back to Default when the
// caller omits a value, and producing an error when a Required parameter
// has neither a caller value nor a Default.
type TemplateParam struct {
	// Name is the parameter name. It corresponds to a placeholder in the
	// template content, e.g. {{.target}} for Name=="target".
	Name string `json:"name"`

	// Description is a human-readable description of the parameter.
	Description string `json:"description"`

	// Required indicates whether the caller must supply a value for this
	// parameter. If true and no value and no Default is provided,
	// instantiation fails.
	Required bool `json:"required"`

	// Default is the default value used when the caller omits the
	// parameter. May be empty.
	Default string `json:"default,omitempty"`

	// Type is the parameter type. Supported values: "string", "int",
	// "bool", "list". Unknown values are treated as "string".
	Type string `json:"type,omitempty"`
}

// --- Template ---------------------------------------------------------------

// Template is a parameterized workflow definition.
//
// Content holds the YAML workflow document with Go-template-style
// placeholders (e.g. {{.target}}). Parameters describes each placeholder.
// Tags is an arbitrary key/value map for categorization (e.g.
// {"team":"sre","env":"prod"}).
type Template struct {
	// ID is the unique template identifier, generated on first Save.
	// It is stable across updates.
	ID string `json:"id"`

	// Name is the human-readable template name and the primary lookup
	// key. Must be unique within a library.
	Name string `json:"name"`

	// Description is a short human-readable description of the template.
	Description string `json:"description"`

	// Content is the YAML workflow definition, possibly containing
	// placeholders like {{.target}}.
	Content string `json:"content"`

	// Parameters is the list of declared placeholder parameters.
	Parameters []TemplateParam `json:"parameters,omitempty"`

	// Tags is an arbitrary key/value map for categorization.
	Tags map[string]string `json:"tags,omitempty"`

	// CreatedAt is the template creation timestamp (UTC).
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is the timestamp of the last update, or nil if never
	// updated.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// --- TemplateUpdate ---------------------------------------------------------

// TemplateUpdate carries optional field updates for Update. Each field is a
// pointer: a nil pointer means "do not change this field", while a non-nil
// pointer means "set this field to the pointed-to value" (which may itself
// be the zero value, e.g. an empty string to clear a description).
type TemplateUpdate struct {
	// Description, when non-nil, replaces the template's Description.
	Description *string

	// Content, when non-nil, replaces the template's Content. Must be
	// non-empty.
	Content *string

	// Parameters, when non-nil, replaces the template's Parameters.
	Parameters *[]TemplateParam

	// Tags, when non-nil, replaces the template's Tags.
	Tags *map[string]string
}

// --- TemplateLibrary --------------------------------------------------------

// TemplateLibrary manages the CRUD lifecycle of workflow templates.
//
// Templates are persisted as one JSON file per template under baseDir,
// named "<name>.json". The library holds a sync.RWMutex to serialize file
// access, making it safe for concurrent use.
type TemplateLibrary struct {
	baseDir string
	mu      sync.RWMutex
}

// NewTemplateLibrary returns a TemplateLibrary backed by the given base
// directory. The directory is created (with mode 0o755) if it does not
// already exist. A non-empty baseDir is required.
func NewTemplateLibrary(baseDir string) (*TemplateLibrary, error) {
	if baseDir == "" {
		return nil, fmt.Errorf("template: empty base dir")
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("template: create base dir: %w", err)
	}
	return &TemplateLibrary{baseDir: baseDir}, nil
}

// Save persists a template, creating it if new or updating it if it already
// exists (matched by Name).
//
// On create:
//   - A fresh ID is generated (crypto/rand + hex, prefixed "tmpl-").
//   - CreatedAt is set to now (UTC) if zero.
//   - UpdatedAt is left nil.
//
// On update (a template with the same Name already exists on disk):
//   - The existing ID and CreatedAt are preserved.
//   - UpdatedAt is set to now (UTC).
//   - The caller's ID, if set, must match the existing ID; otherwise
//     ErrTemplateExists is returned (the name is owned by a different
//     template).
//
// Validation:
//   - Name must be non-empty (ErrEmptyTemplateName).
//   - Content must be non-empty (ErrEmptyContent).
func (l *TemplateLibrary) Save(ctx context.Context, tmpl *Template) error {
	if tmpl == nil {
		return fmt.Errorf("%w: nil template", ErrInvalidTemplate)
	}
	if tmpl.Name == "" {
		return ErrEmptyTemplateName
	}
	if tmpl.Content == "" {
		return ErrEmptyContent
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now().UTC()

	existing, err := l.loadLocked(tmpl.Name)
	if err != nil && !errors.Is(err, ErrTemplateNotFound) {
		return fmt.Errorf("template: stat existing: %w", err)
	}

	if existing == nil {
		// Create.
		id, err := newTemplateID()
		if err != nil {
			return fmt.Errorf("template: generate id: %w", err)
		}
		tmpl.ID = id
		if tmpl.CreatedAt.IsZero() {
			tmpl.CreatedAt = now
		}
		tmpl.UpdatedAt = nil
	} else {
		// Update by re-Save: preserve ID and CreatedAt, bump UpdatedAt.
		if tmpl.ID != "" && tmpl.ID != existing.ID {
			return fmt.Errorf("%w: name %q owned by id %s", ErrTemplateExists, tmpl.Name, existing.ID)
		}
		tmpl.ID = existing.ID
		if tmpl.CreatedAt.IsZero() {
			tmpl.CreatedAt = existing.CreatedAt
		}
		tmpl.UpdatedAt = &now
	}

	if err := l.writeLocked(tmpl); err != nil {
		return fmt.Errorf("template: write %s: %w", tmpl.Name, err)
	}
	return nil
}

// Get returns the template with the given name.
// Returns ErrTemplateNotFound when no such template exists.
func (l *TemplateLibrary) Get(ctx context.Context, name string) (*Template, error) {
	if name == "" {
		return nil, ErrEmptyTemplateName
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.loadLocked(name)
}

// List returns all templates in the library, sorted by name ascending.
// Returns an empty slice (not nil) when the library is empty.
func (l *TemplateLibrary) List(ctx context.Context) ([]*Template, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	entries, err := os.ReadDir(l.baseDir)
	if err != nil {
		return nil, fmt.Errorf("template: read dir: %w", err)
	}

	out := make([]*Template, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		tmpl, err := l.loadLocked(name)
		if err != nil {
			// Skip entries that cannot be decoded rather than failing the
			// whole listing; this keeps List resilient to stray files.
			continue
		}
		out = append(out, tmpl)
	}
	// Sort by name ascending for stable output.
	sortTemplatesByName(out)
	return out, nil
}

// Delete removes the template with the given name.
// Returns ErrTemplateNotFound when no such template exists.
func (l *TemplateLibrary) Delete(ctx context.Context, name string) error {
	if name == "" {
		return ErrEmptyTemplateName
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	path := l.templatePath(name)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return ErrTemplateNotFound
		}
		return fmt.Errorf("template: stat %s: %w", name, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("template: remove %s: %w", name, err)
	}
	return nil
}

// Show returns a human-readable, formatted description of the template,
// suitable for CLI display. The format includes the template's metadata
// (ID, name, description, tags, timestamps), its parameter definitions and
// its content.
//
// Returns ErrTemplateNotFound when no such template exists.
func (l *TemplateLibrary) Show(ctx context.Context, name string) (string, error) {
	tmpl, err := l.Get(ctx, name)
	if err != nil {
		return "", err
	}
	return formatTemplate(tmpl), nil
}

// Update applies the given non-nil field updates to the template identified
// by name and returns the updated template.
//
// Fields whose pointers are nil are left unchanged. Content, when provided,
// must be non-empty (ErrEmptyContent).
//
// Returns ErrTemplateNotFound when no such template exists.
func (l *TemplateLibrary) Update(ctx context.Context, name string, updates TemplateUpdate) (*Template, error) {
	if name == "" {
		return nil, ErrEmptyTemplateName
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	tmpl, err := l.loadLocked(name)
	if err != nil {
		return nil, err
	}

	if updates.Description != nil {
		tmpl.Description = *updates.Description
	}
	if updates.Content != nil {
		if *updates.Content == "" {
			return nil, ErrEmptyContent
		}
		tmpl.Content = *updates.Content
	}
	if updates.Parameters != nil {
		tmpl.Parameters = *updates.Parameters
	}
	if updates.Tags != nil {
		tmpl.Tags = *updates.Tags
	}

	now := time.Now().UTC()
	tmpl.UpdatedAt = &now

	if err := l.writeLocked(tmpl); err != nil {
		return nil, fmt.Errorf("template: write %s: %w", name, err)
	}
	return tmpl, nil
}

// --- internal helpers -------------------------------------------------------

// templatePath returns the on-disk path for a template name.
func (l *TemplateLibrary) templatePath(name string) string {
	return filepath.Join(l.baseDir, name+".json")
}

// loadLocked reads and decodes a template by name. Caller must hold at least
// l.mu.RLock.
func (l *TemplateLibrary) loadLocked(name string) (*Template, error) {
	path := l.templatePath(name)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrTemplateNotFound
		}
		return nil, fmt.Errorf("template: read %s: %w", name, err)
	}
	var tmpl Template
	if err := json.Unmarshal(data, &tmpl); err != nil {
		return nil, fmt.Errorf("template: decode %s: %w", name, err)
	}
	return &tmpl, nil
}

// writeLocked encodes and writes a template to disk. Caller must hold
// l.mu.Lock.
func (l *TemplateLibrary) writeLocked(tmpl *Template) error {
	data, err := json.MarshalIndent(tmpl, "", "  ")
	if err != nil {
		return fmt.Errorf("template: encode %s: %w", tmpl.Name, err)
	}
	path := l.templatePath(tmpl.Name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("template: write file %s: %w", tmpl.Name, err)
	}
	return nil
}

// newTemplateID generates a fresh template ID: "tmpl-" + 16 hex chars.
func newTemplateID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "tmpl-" + hex.EncodeToString(b), nil
}

// sortTemplatesByName sorts a slice of templates by Name ascending.
// A simple insertion sort is used because the slice is small in practice.
func sortTemplatesByName(ts []*Template) {
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0 && ts[j-1].Name > ts[j].Name; j-- {
			ts[j-1], ts[j] = ts[j], ts[j-1]
		}
	}
}

// formatTemplate renders a template as a human-readable string.
func formatTemplate(t *Template) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ID:          %s\n", t.ID)
	fmt.Fprintf(&b, "Name:        %s\n", t.Name)
	fmt.Fprintf(&b, "Description: %s\n", t.Description)
	fmt.Fprintf(&b, "Created:     %s\n", t.CreatedAt.UTC().Format(time.RFC3339))
	if t.UpdatedAt != nil {
		fmt.Fprintf(&b, "Updated:     %s\n", t.UpdatedAt.UTC().Format(time.RFC3339))
	} else {
		b.WriteString("Updated:     -\n")
	}

	// Tags
	b.WriteString("Tags:\n")
	if len(t.Tags) == 0 {
		b.WriteString("  (none)\n")
	} else {
		// Stable order: collect keys and sort.
		keys := make([]string, 0, len(t.Tags))
		for k := range t.Tags {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "  %s: %s\n", k, t.Tags[k])
		}
	}

	// Parameters
	b.WriteString("Parameters:\n")
	if len(t.Parameters) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, p := range t.Parameters {
			req := "optional"
			if p.Required {
				req = "required"
			}
			ptype := p.Type
			if ptype == "" {
				ptype = "string"
			}
			fmt.Fprintf(&b, "  - %s (%s, %s)", p.Name, ptype, req)
			if p.Default != "" {
				fmt.Fprintf(&b, " default=%q", p.Default)
			}
			b.WriteByte('\n')
			if p.Description != "" {
				fmt.Fprintf(&b, "      %s\n", p.Description)
			}
		}
	}

	// Content
	b.WriteString("Content:\n")
	b.WriteString(t.Content)
	if !strings.HasSuffix(t.Content, "\n") {
		b.WriteByte('\n')
	}
	return b.String()
}

// sortStrings sorts a string slice ascending (insertion sort).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
