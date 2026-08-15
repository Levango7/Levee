// Package grpc implements the LEVEE gRPC service handlers.
//
// This file implements TemplateService, which manages the workflow template
// library: creating, retrieving, listing, deleting templates and
// instantiating a template into a concrete change (Run). Template CRUD is
// backed by an in-memory registry when no template.Library is configured,
// which keeps tests hermetic and avoids file-system dependencies.
package grpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/template"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// TemplateService implements pb.TemplateServiceServer. It manages workflow
// templates and instantiates them into changes (state.Run records).
//
// When lib is non-nil, template CRUD delegates to the file-backed
// template.TemplateLibrary. When lib is nil, an in-memory registry is used,
// which is convenient for tests and ephemeral deployments.
type TemplateService struct {
	pb.UnimplementedTemplateServiceServer

	store state.Store
	lib   *template.TemplateLibrary

	mu        sync.RWMutex
	templates map[string]*pb.Template // in-memory registry, keyed by name
}

// NewTemplateService returns a TemplateService backed by the given store and
// template library. If lib is nil, an in-memory template registry is used.
// The store must be non-nil; it is used by InstantiateTemplate to create the
// resulting Run.
func NewTemplateService(store state.Store, lib *template.TemplateLibrary) *TemplateService {
	return &TemplateService{
		store:     store,
		lib:       lib,
		templates: make(map[string]*pb.Template),
	}
}

// CreateTemplate creates a new template or, when Overwrite is true, replaces an
// existing template with the same name.
func (s *TemplateService) CreateTemplate(ctx context.Context, req *pb.CreateTemplateRequest) (*pb.Template, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}
	if strings.TrimSpace(req.Name) == "" {
		return nil, status.Error(codes.InvalidArgument, "template name is required")
	}
	if req.WorkflowContent == "" {
		return nil, status.Error(codes.InvalidArgument, "workflow content is required")
	}

	if s.lib != nil {
		return s.createTemplateViaLibrary(ctx, req)
	}

	// In-memory path.
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.templates[req.Name]
	if ok && !req.Overwrite {
		return nil, status.Errorf(codes.AlreadyExists, "template %q already exists", req.Name)
	}

	now := time.Now().UTC().Unix()
	tmpl := &pb.Template{
		Name:            req.Name,
		Description:     req.Description,
		WorkflowContent: req.WorkflowContent,
		RequiredParams:  req.RequiredParams,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if ok && existing != nil {
		tmpl.CreatedAt = existing.CreatedAt
	}
	s.templates[req.Name] = tmpl
	return tmpl, nil
}

// createTemplateViaLibrary delegates template creation to the file-backed
// template.TemplateLibrary and converts the result back to a pb.Template.
func (s *TemplateService) createTemplateViaLibrary(ctx context.Context, req *pb.CreateTemplateRequest) (*pb.Template, error) {
	params := make([]template.TemplateParam, 0, len(req.RequiredParams))
	for _, name := range req.RequiredParams {
		params = append(params, template.TemplateParam{Name: name, Required: true, Type: "string"})
	}

	tmpl := &template.Template{
		Name:        req.Name,
		Description: req.Description,
		Content:     req.WorkflowContent,
		Parameters:  params,
	}

	if req.Overwrite {
		// Save handles both create and update by name.
		if err := s.lib.Save(ctx, tmpl); err != nil {
			return nil, status.Errorf(codes.Internal, "save template: %v", err)
		}
	} else {
		// Check existence first to honour the Overwrite flag semantics.
		if _, err := s.lib.Get(ctx, req.Name); err == nil {
			return nil, status.Errorf(codes.AlreadyExists, "template %q already exists", req.Name)
		} else if !errors.Is(err, template.ErrTemplateNotFound) {
			return nil, status.Errorf(codes.Internal, "check template: %v", err)
		}
		if err := s.lib.Save(ctx, tmpl); err != nil {
			return nil, status.Errorf(codes.Internal, "save template: %v", err)
		}
	}
	return domainTemplateToPB(tmpl), nil
}

// GetTemplate returns a single template by name.
func (s *TemplateService) GetTemplate(ctx context.Context, req *pb.GetTemplateRequest) (*pb.Template, error) {
	if req == nil || strings.TrimSpace(req.Name) == "" {
		return nil, status.Error(codes.InvalidArgument, "template name is required")
	}

	if s.lib != nil {
		tmpl, err := s.lib.Get(ctx, req.Name)
		if err != nil {
			if errors.Is(err, template.ErrTemplateNotFound) {
				return nil, status.Errorf(codes.NotFound, "template %q not found", req.Name)
			}
			return nil, status.Errorf(codes.Internal, "get template: %v", err)
		}
		return domainTemplateToPB(tmpl), nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	tmpl, ok := s.templates[req.Name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "template %q not found", req.Name)
	}
	return cloneTemplate(tmpl), nil
}

// ListTemplates returns templates matching the given filter, with pagination.
func (s *TemplateService) ListTemplates(ctx context.Context, req *pb.ListTemplatesRequest) (*pb.ListTemplatesResponse, error) {
	if req == nil {
		req = &pb.ListTemplatesRequest{}
	}

	pageSize := int(req.PageSize)
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	offset := parsePageToken(req.PageToken)

	var all []*pb.Template
	if s.lib != nil {
		libTmpls, err := s.lib.List(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list templates: %v", err)
		}
		for _, t := range libTmpls {
			all = append(all, domainTemplateToPB(t))
		}
	} else {
		s.mu.RLock()
		for _, t := range s.templates {
			all = append(all, t)
		}
		s.mu.RUnlock()
	}

	// Filter by name substring.
	if req.NameContains != "" {
		filtered := make([]*pb.Template, 0, len(all))
		for _, t := range all {
			if strings.Contains(t.Name, req.NameContains) {
				filtered = append(filtered, t)
			}
		}
		all = filtered
	}

	// Sort by name for stable output.
	sortTemplatesByName(all)

	total := len(all)
	if offset > total {
		offset = total
	}
	end := offset + pageSize
	if end > total {
		end = total
	}
	page := all[offset:end]

	resp := &pb.ListTemplatesResponse{
		Templates:     page,
		TotalSize:     int32(total),
		NextPageToken: buildPageToken(end, total),
	}
	return resp, nil
}

// DeleteTemplate removes a template by name. The Force flag is accepted but
// currently a no-op for the in-memory backend; the library backend always
// deletes.
func (s *TemplateService) DeleteTemplate(ctx context.Context, req *pb.DeleteTemplateRequest) (*emptypb.Empty, error) {
	if req == nil || strings.TrimSpace(req.Name) == "" {
		return nil, status.Error(codes.InvalidArgument, "template name is required")
	}

	if s.lib != nil {
		if err := s.lib.Delete(ctx, req.Name); err != nil {
			if errors.Is(err, template.ErrTemplateNotFound) {
				return nil, status.Errorf(codes.NotFound, "template %q not found", req.Name)
			}
			return nil, status.Errorf(codes.Internal, "delete template: %v", err)
		}
		return &emptypb.Empty{}, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.templates[req.Name]; !ok {
		return nil, status.Errorf(codes.NotFound, "template %q not found", req.Name)
	}
	delete(s.templates, req.Name)
	return &emptypb.Empty{}, nil
}

// InstantiateTemplate resolves a template's parameters and creates a new
// change (state.Run) from the resulting workflow content. When DryRun is true
// the Run is created in "planned" status without executing it.
func (s *TemplateService) InstantiateTemplate(ctx context.Context, req *pb.InstantiateTemplateRequest) (*pb.Change, error) {
	if req == nil || strings.TrimSpace(req.TemplateName) == "" {
		return nil, status.Error(codes.InvalidArgument, "template name is required")
	}
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "store not configured")
	}

	// 1. Load template.
	var tmplContent string
	var requiredParams []string
	if s.lib != nil {
		tmpl, err := s.lib.Get(ctx, req.TemplateName)
		if err != nil {
			if errors.Is(err, template.ErrTemplateNotFound) {
				return nil, status.Errorf(codes.NotFound, "template %q not found", req.TemplateName)
			}
			return nil, status.Errorf(codes.Internal, "get template: %v", err)
		}
		tmplContent = tmpl.Content
		for _, p := range tmpl.Parameters {
			if p.Required {
				requiredParams = append(requiredParams, p.Name)
			}
		}
		// Use the instantiator for proper parameter resolution.
		inst := template.NewInstantiator()
		result, err := inst.Instantiate(tmpl, req.Params)
		if err != nil {
			if errors.Is(err, template.ErrRequiredParamMissing) {
				return nil, status.Errorf(codes.InvalidArgument, "missing required parameters: %v", err)
			}
			return nil, status.Errorf(codes.InvalidArgument, "instantiate: %v", err)
		}
		tmplContent = result.Content
	} else {
		s.mu.RLock()
		tmpl, ok := s.templates[req.TemplateName]
		s.mu.RUnlock()
		if !ok {
			return nil, status.Errorf(codes.NotFound, "template %q not found", req.TemplateName)
		}
		tmplContent = tmpl.WorkflowContent
		requiredParams = tmpl.RequiredParams

		// Validate required parameters.
		var missing []string
		for _, p := range requiredParams {
			if _, ok := req.Params[p]; !ok {
				missing = append(missing, p)
			}
		}
		if len(missing) > 0 {
			return nil, status.Errorf(codes.InvalidArgument, "missing required parameters: %s", strings.Join(missing, ", "))
		}
		// Substitute placeholders.
		for k, v := range req.Params {
			tmplContent = strings.ReplaceAll(tmplContent, "{{."+k+"}}", v)
		}
	}

	// 2. Create the Run.
	runID, err := generateID("run-")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate run id: %v", err)
	}

	priority := req.Priority
	if priority == "" {
		priority = "normal"
	}
	runStatus := "planned"
	if !req.DryRun {
		runStatus = "pending_approval"
	}

	now := time.Now().UTC()
	run := &state.Run{
		ID:             runID,
		WorkflowName:   tmplContent,
		TemplateName:   req.TemplateName,
		Params:         paramsToJSON(req.Params),
		Status:         runStatus,
		ApprovalStatus: "pending",
		CreatedAt:      now,
		UpdatedAt:      now,
		Creator:        "grpc",
	}
	if err := s.store.CreateRun(ctx, run); err != nil {
		return nil, status.Errorf(codes.Internal, "create run: %v", err)
	}

	return &pb.Change{
		Id:           run.ID,
		Label:        req.Label,
		Status:       run.Status,
		Priority:     priority,
		WorkflowFile: "",
		TemplateName: req.TemplateName,
		Params:       req.Params,
		CreatedAt:    now.Unix(),
		UpdatedAt:    now.Unix(),
		CreatedBy:    "grpc",
		Team:         req.Team,
		Environment:  req.Environment,
	}, nil
}

// --- helpers -----------------------------------------------------------------

// domainTemplateToPB converts a template.Template to a pb.Template.
func domainTemplateToPB(t *template.Template) *pb.Template {
	if t == nil {
		return nil
	}
	pbTmpl := &pb.Template{
		Name:            t.Name,
		Description:     t.Description,
		WorkflowContent: t.Content,
		CreatedAt:       t.CreatedAt.Unix(),
	}
	if t.UpdatedAt != nil {
		pbTmpl.UpdatedAt = t.UpdatedAt.Unix()
	}
	for _, p := range t.Parameters {
		if p.Required {
			pbTmpl.RequiredParams = append(pbTmpl.RequiredParams, p.Name)
		}
	}
	return pbTmpl
}

// cloneTemplate returns a shallow copy of a pb.Template so callers cannot
// mutate the registry entry.
func cloneTemplate(t *pb.Template) *pb.Template {
	if t == nil {
		return nil
	}
	cp := &pb.Template{
		Name:            t.Name,
		Description:     t.Description,
		WorkflowContent: t.WorkflowContent,
		CreatedAt:       t.CreatedAt,
		UpdatedAt:       t.UpdatedAt,
	}
	if t.RequiredParams != nil {
		cp.RequiredParams = append([]string(nil), t.RequiredParams...)
	}
	return cp
}

// sortTemplatesByName sorts a slice of pb.Template by Name ascending.
func sortTemplatesByName(ts []*pb.Template) {
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0 && ts[j-1].Name > ts[j].Name; j-- {
			ts[j-1], ts[j] = ts[j], ts[j-1]
		}
	}
}

// generateID returns a prefix + 16 hex chars random ID.
func generateID(prefix string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}

// paramsToJSON encodes a string map as a JSON object string. Returns "{}" for
// nil/empty input so the stored value is always valid JSON.
func paramsToJSON(params map[string]string) string {
	if len(params) == 0 {
		return "{}"
	}
	var b strings.Builder
	b.WriteByte('{')
	first := true
	// Stable order is not required for JSON but keeps diffs reproducible.
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	for _, k := range keys {
		if !first {
			b.WriteByte(',')
		}
		first = false
		fmt.Fprintf(&b, "%q:%q", k, params[k])
	}
	b.WriteByte('}')
	return b.String()
}
