// service_factory.go implements the dual-mode Service factory that backs every
// CLI command. Depending on the --local / --remote flag the factory returns
// either:
//
//   - modeLocal:  in-process service implementations backed directly by a
//                 state.Store. This is the default and keeps the CLI a single
//                 zero-dependency binary for development and air-gapped use.
//   - modeRemote: thin adapters around the generated gRPC clients, forwarding
//                 every call to a remote LEVEE server over the network.
//
// Both modes expose the same *ServiceAdapter interfaces, so command code is
// identical regardless of where the work actually runs.
//
// Streaming RPCs (WatchChange, StreamLogs) are intentionally omitted from the
// adapter interfaces: CLI commands consume state synchronously and do not
// currently need server streaming. They can be added later by introducing a
// small streaming abstraction without breaking call sites.

package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/template"

	"google.golang.org/protobuf/types/known/emptypb"
)

// serviceMode selects whether the CLI talks to an in-process store or a remote
// gRPC server.
type serviceMode int

const (
	// modeLocal routes every call to in-process service implementations
	// backed by a state.Store.
	modeLocal serviceMode = iota
	// modeRemote routes every call through gRPC clients to a remote server.
	modeRemote
)

// String returns a human-readable mode name for logging and diagnostics.
func (m serviceMode) String() string {
	switch m {
	case modeLocal:
		return "local"
	case modeRemote:
		return "remote"
	default:
		return "unknown"
	}
}

// serviceFactory holds the wiring needed to produce service adapters. Only one
// of local / remote is populated depending on the active mode.
type serviceFactory struct {
	mode   serviceMode
	local  *localServices
	remote *grpcClient
}

// localServices bundles the in-process service implementations. Each field is
// backed by the same state.Store so that a CLI process sees a single
// consistent view.
type localServices struct {
	change   *grpc.ChangeService
	template *grpc.TemplateService
	target   *grpc.TargetService
	audit    *grpc.AuditService
	system   *grpc.SystemService
}

// localDeps carries the optional internal dependencies that the local service
// implementations can use. Remote mode ignores this entirely. Every field is
// optional; nil falls back to a store-only / reduced-functionality code path.
type localDeps struct {
	store          state.Store
	engine         *grpc.EngineAdapter
	approval       *approval.Service
	pause          *pauseManagerShim
	templateLib    *template.TemplateLibrary
	channelFactory channel.ChannelFactory
	config         *config.Config
	configPath     string
}

// pauseManagerShim is a thin alias to avoid importing internal/pause here when
// callers do not need it. The field is typed as the concrete *pause.PauseManager
// at the call site via a small wrapper; we keep the indirection so this file
// does not import internal/pause directly (which would add a build-time
// dependency cycle in some test configurations).
type pauseManagerShim = struct{}

// factoryOption configures newServiceFactory.
type factoryOption func(*factoryConfig)

// factoryConfig is the internal configuration bag.
type factoryConfig struct {
	deps           *localDeps
	tlsConfig      *tlsConfigShim
	connectTimeout time.Duration
}

// tlsConfigShim is an alias to keep the import surface small; the real type is
// *tls.Config, supplied via WithFactoryTLS.
type tlsConfigShim = struct{}

// WithFactoryDeps supplies the local-mode dependencies. Ignored in remote mode
// except for store, which is unused remotely.
func WithFactoryDeps(deps *localDeps) factoryOption {
	return func(c *factoryConfig) {
		c.deps = deps
	}
}

// newServiceFactory builds a serviceFactory for the requested mode.
//
//   - modeLocal:  requires a non-nil store (via deps); constructs the five
//     in-process service implementations.
//   - modeRemote: dials remoteAddr and builds a grpcClient. token may be empty
//     to disable authentication.
//
// The returned factory must be closed via Close when the caller is done to
// release any underlying connection.
func newServiceFactory(mode serviceMode, remoteAddr string, token string, opts ...factoryOption) (*serviceFactory, error) {
	cfg := &factoryConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	f := &serviceFactory{mode: mode}

	switch mode {
	case modeLocal:
		if cfg.deps == nil || cfg.deps.store == nil {
			return nil, errors.New("service factory: local mode requires a non-nil store")
		}
		f.local = buildLocalServices(cfg.deps)

	case modeRemote:
		client, err := newGRPCClient(remoteAddr, token)
		if err != nil {
			return nil, fmt.Errorf("service factory: remote mode: %w", err)
		}
		f.remote = client

	default:
		return nil, fmt.Errorf("service factory: unknown mode %d", mode)
	}

	return f, nil
}

// buildLocalServices constructs the five in-process service implementations
// from the supplied dependencies. Optional dependencies fall back to nil.
func buildLocalServices(deps *localDeps) *localServices {
	ls := &localServices{
		change:   grpc.NewChangeService(deps.store, deps.engine, deps.approval, nil),
		template: grpc.NewTemplateService(deps.store, deps.templateLib),
		target:   grpc.NewTargetService(deps.channelFactory),
		audit:    grpc.NewAuditService(deps.store),
	}
	if deps.config != nil {
		ls.system = grpc.NewSystemService(
			deps.store, deps.config, deps.configPath,
			version, commitHash, buildTime, goVersion, time.Now(),
		)
	} else {
		// Without a config we still construct a SystemService so that
		// `levee system version` works in degraded mode; pass an empty
		// config and path.
		ls.system = grpc.NewSystemService(
			deps.store, &config.Config{}, "",
			version, commitHash, buildTime, goVersion, time.Now(),
		)
	}
	return ls
}

// Close releases any resources held by the factory. In local mode it is a
// no-op (the store is owned by the caller); in remote mode it closes the
// gRPC connection.
func (f *serviceFactory) Close() error {
	if f == nil {
		return nil
	}
	if f.remote != nil {
		return f.remote.close()
	}
	return nil
}

// Mode returns the active service mode.
func (f *serviceFactory) Mode() serviceMode { return f.mode }

// --- ChangeService adapter --------------------------------------------------

// changeServiceAdapter unifies the local ChangeService implementation and the
// remote ChangeServiceClient so that command code is identical in both modes.
// Streaming RPCs (WatchChange, StreamLogs) are intentionally omitted.
type changeServiceAdapter interface {
	CreateChange(ctx context.Context, req *pb.CreateChangeRequest) (*pb.Change, error)
	CloneChange(ctx context.Context, req *pb.CloneChangeRequest) (*pb.Change, error)
	PlanChange(ctx context.Context, req *pb.PlanChangeRequest) (*pb.Plan, error)
	ApplyChange(ctx context.Context, req *pb.ApplyChangeRequest) (*pb.ApplyResponse, error)
	PauseChange(ctx context.Context, req *pb.PauseRequest) (*pb.Change, error)
	ResumeChange(ctx context.Context, req *pb.PauseRequest) (*pb.Change, error)
	PauseAll(ctx context.Context, req *pb.PauseAllRequest) (*pb.PauseAllResponse, error)
	ResumeAll(ctx context.Context, req *pb.PauseAllRequest) (*pb.PauseAllResponse, error)
	CancelChange(ctx context.Context, req *pb.CancelRequest) (*pb.Change, error)
	RetryChange(ctx context.Context, req *pb.RetryRequest) (*pb.Change, error)
	RetryHost(ctx context.Context, req *pb.RetryHostRequest) (*pb.Change, error)
	RollbackChange(ctx context.Context, req *pb.RollbackRequest) (*pb.RollbackResponse, error)
	ApproveChange(ctx context.Context, req *pb.ApproveRequest) (*pb.Change, error)
	RejectChange(ctx context.Context, req *pb.RejectRequest) (*pb.Change, error)
	GetChange(ctx context.Context, req *pb.GetChangeRequest) (*pb.Change, error)
	ListChanges(ctx context.Context, req *pb.ListChangesRequest) (*pb.ListChangesResponse, error)
	ArchiveChange(ctx context.Context, req *pb.ArchiveRequest) (*pb.Change, error)
	GetLogs(ctx context.Context, req *pb.GetLogsRequest) (*pb.GetLogsResponse, error)
	GetDiff(ctx context.Context, req *pb.GetDiffRequest) (*pb.GetDiffResponse, error)
	GetTrace(ctx context.Context, req *pb.GetTraceRequest) (*pb.GetTraceResponse, error)
}

// GetChangeService returns a ChangeService adapter for the active mode.
func (f *serviceFactory) GetChangeService() changeServiceAdapter {
	if f.mode == modeRemote {
		return &remoteChangeAdapter{client: f.remote.change}
	}
	return &localChangeAdapter{svc: f.local.change}
}

// localChangeAdapter wraps a *grpc.ChangeService to satisfy
// changeServiceAdapter. The underlying type already implements every method
// with the matching signature, so the wrapper is just a type-level bridge.
type localChangeAdapter struct {
	svc *grpc.ChangeService
}

// remoteChangeAdapter wraps a pb.ChangeServiceClient to satisfy
// changeServiceAdapter by forwarding each call without CallOptions.
type remoteChangeAdapter struct {
	client pb.ChangeServiceClient
}

// The generated local implementation already has all methods with the exact
// signatures required by changeServiceAdapter, so we delegate directly.

func (a *localChangeAdapter) CreateChange(ctx context.Context, req *pb.CreateChangeRequest) (*pb.Change, error) {
	return a.svc.CreateChange(ctx, req)
}
func (a *localChangeAdapter) CloneChange(ctx context.Context, req *pb.CloneChangeRequest) (*pb.Change, error) {
	return a.svc.CloneChange(ctx, req)
}
func (a *localChangeAdapter) PlanChange(ctx context.Context, req *pb.PlanChangeRequest) (*pb.Plan, error) {
	return a.svc.PlanChange(ctx, req)
}
func (a *localChangeAdapter) ApplyChange(ctx context.Context, req *pb.ApplyChangeRequest) (*pb.ApplyResponse, error) {
	return a.svc.ApplyChange(ctx, req)
}
func (a *localChangeAdapter) PauseChange(ctx context.Context, req *pb.PauseRequest) (*pb.Change, error) {
	return a.svc.PauseChange(ctx, req)
}
func (a *localChangeAdapter) ResumeChange(ctx context.Context, req *pb.PauseRequest) (*pb.Change, error) {
	return a.svc.ResumeChange(ctx, req)
}
func (a *localChangeAdapter) PauseAll(ctx context.Context, req *pb.PauseAllRequest) (*pb.PauseAllResponse, error) {
	return a.svc.PauseAll(ctx, req)
}
func (a *localChangeAdapter) ResumeAll(ctx context.Context, req *pb.PauseAllRequest) (*pb.PauseAllResponse, error) {
	return a.svc.ResumeAll(ctx, req)
}
func (a *localChangeAdapter) CancelChange(ctx context.Context, req *pb.CancelRequest) (*pb.Change, error) {
	return a.svc.CancelChange(ctx, req)
}
func (a *localChangeAdapter) RetryChange(ctx context.Context, req *pb.RetryRequest) (*pb.Change, error) {
	return a.svc.RetryChange(ctx, req)
}
func (a *localChangeAdapter) RetryHost(ctx context.Context, req *pb.RetryHostRequest) (*pb.Change, error) {
	return a.svc.RetryHost(ctx, req)
}
func (a *localChangeAdapter) RollbackChange(ctx context.Context, req *pb.RollbackRequest) (*pb.RollbackResponse, error) {
	return a.svc.RollbackChange(ctx, req)
}
func (a *localChangeAdapter) ApproveChange(ctx context.Context, req *pb.ApproveRequest) (*pb.Change, error) {
	return a.svc.ApproveChange(ctx, req)
}
func (a *localChangeAdapter) RejectChange(ctx context.Context, req *pb.RejectRequest) (*pb.Change, error) {
	return a.svc.RejectChange(ctx, req)
}
func (a *localChangeAdapter) GetChange(ctx context.Context, req *pb.GetChangeRequest) (*pb.Change, error) {
	return a.svc.GetChange(ctx, req)
}
func (a *localChangeAdapter) ListChanges(ctx context.Context, req *pb.ListChangesRequest) (*pb.ListChangesResponse, error) {
	return a.svc.ListChanges(ctx, req)
}
func (a *localChangeAdapter) ArchiveChange(ctx context.Context, req *pb.ArchiveRequest) (*pb.Change, error) {
	return a.svc.ArchiveChange(ctx, req)
}
func (a *localChangeAdapter) GetLogs(ctx context.Context, req *pb.GetLogsRequest) (*pb.GetLogsResponse, error) {
	return a.svc.GetLogs(ctx, req)
}
func (a *localChangeAdapter) GetDiff(ctx context.Context, req *pb.GetDiffRequest) (*pb.GetDiffResponse, error) {
	return a.svc.GetDiff(ctx, req)
}
func (a *localChangeAdapter) GetTrace(ctx context.Context, req *pb.GetTraceRequest) (*pb.GetTraceResponse, error) {
	return a.svc.GetTrace(ctx, req)
}

func (a *remoteChangeAdapter) CreateChange(ctx context.Context, req *pb.CreateChangeRequest) (*pb.Change, error) {
	return a.client.CreateChange(ctx, req)
}
func (a *remoteChangeAdapter) CloneChange(ctx context.Context, req *pb.CloneChangeRequest) (*pb.Change, error) {
	return a.client.CloneChange(ctx, req)
}
func (a *remoteChangeAdapter) PlanChange(ctx context.Context, req *pb.PlanChangeRequest) (*pb.Plan, error) {
	return a.client.PlanChange(ctx, req)
}
func (a *remoteChangeAdapter) ApplyChange(ctx context.Context, req *pb.ApplyChangeRequest) (*pb.ApplyResponse, error) {
	return a.client.ApplyChange(ctx, req)
}
func (a *remoteChangeAdapter) PauseChange(ctx context.Context, req *pb.PauseRequest) (*pb.Change, error) {
	return a.client.PauseChange(ctx, req)
}
func (a *remoteChangeAdapter) ResumeChange(ctx context.Context, req *pb.PauseRequest) (*pb.Change, error) {
	return a.client.ResumeChange(ctx, req)
}
func (a *remoteChangeAdapter) PauseAll(ctx context.Context, req *pb.PauseAllRequest) (*pb.PauseAllResponse, error) {
	return a.client.PauseAll(ctx, req)
}
func (a *remoteChangeAdapter) ResumeAll(ctx context.Context, req *pb.PauseAllRequest) (*pb.PauseAllResponse, error) {
	return a.client.ResumeAll(ctx, req)
}
func (a *remoteChangeAdapter) CancelChange(ctx context.Context, req *pb.CancelRequest) (*pb.Change, error) {
	return a.client.CancelChange(ctx, req)
}
func (a *remoteChangeAdapter) RetryChange(ctx context.Context, req *pb.RetryRequest) (*pb.Change, error) {
	return a.client.RetryChange(ctx, req)
}
func (a *remoteChangeAdapter) RetryHost(ctx context.Context, req *pb.RetryHostRequest) (*pb.Change, error) {
	return a.client.RetryHost(ctx, req)
}
func (a *remoteChangeAdapter) RollbackChange(ctx context.Context, req *pb.RollbackRequest) (*pb.RollbackResponse, error) {
	return a.client.RollbackChange(ctx, req)
}
func (a *remoteChangeAdapter) ApproveChange(ctx context.Context, req *pb.ApproveRequest) (*pb.Change, error) {
	return a.client.ApproveChange(ctx, req)
}
func (a *remoteChangeAdapter) RejectChange(ctx context.Context, req *pb.RejectRequest) (*pb.Change, error) {
	return a.client.RejectChange(ctx, req)
}
func (a *remoteChangeAdapter) GetChange(ctx context.Context, req *pb.GetChangeRequest) (*pb.Change, error) {
	return a.client.GetChange(ctx, req)
}
func (a *remoteChangeAdapter) ListChanges(ctx context.Context, req *pb.ListChangesRequest) (*pb.ListChangesResponse, error) {
	return a.client.ListChanges(ctx, req)
}
func (a *remoteChangeAdapter) ArchiveChange(ctx context.Context, req *pb.ArchiveRequest) (*pb.Change, error) {
	return a.client.ArchiveChange(ctx, req)
}
func (a *remoteChangeAdapter) GetLogs(ctx context.Context, req *pb.GetLogsRequest) (*pb.GetLogsResponse, error) {
	return a.client.GetLogs(ctx, req)
}
func (a *remoteChangeAdapter) GetDiff(ctx context.Context, req *pb.GetDiffRequest) (*pb.GetDiffResponse, error) {
	return a.client.GetDiff(ctx, req)
}
func (a *remoteChangeAdapter) GetTrace(ctx context.Context, req *pb.GetTraceRequest) (*pb.GetTraceResponse, error) {
	return a.client.GetTrace(ctx, req)
}

// --- TemplateService adapter ------------------------------------------------

// templateServiceAdapter unifies local and remote TemplateService.
type templateServiceAdapter interface {
	CreateTemplate(ctx context.Context, req *pb.CreateTemplateRequest) (*pb.Template, error)
	GetTemplate(ctx context.Context, req *pb.GetTemplateRequest) (*pb.Template, error)
	ListTemplates(ctx context.Context, req *pb.ListTemplatesRequest) (*pb.ListTemplatesResponse, error)
	DeleteTemplate(ctx context.Context, req *pb.DeleteTemplateRequest) (*emptypb.Empty, error)
	InstantiateTemplate(ctx context.Context, req *pb.InstantiateTemplateRequest) (*pb.Change, error)
}

// GetTemplateService returns a TemplateService adapter for the active mode.
func (f *serviceFactory) GetTemplateService() templateServiceAdapter {
	if f.mode == modeRemote {
		return &remoteTemplateAdapter{client: f.remote.template}
	}
	return &localTemplateAdapter{svc: f.local.template}
}

type localTemplateAdapter struct {
	svc *grpc.TemplateService
}

type remoteTemplateAdapter struct {
	client pb.TemplateServiceClient
}

func (a *localTemplateAdapter) CreateTemplate(ctx context.Context, req *pb.CreateTemplateRequest) (*pb.Template, error) {
	return a.svc.CreateTemplate(ctx, req)
}
func (a *localTemplateAdapter) GetTemplate(ctx context.Context, req *pb.GetTemplateRequest) (*pb.Template, error) {
	return a.svc.GetTemplate(ctx, req)
}
func (a *localTemplateAdapter) ListTemplates(ctx context.Context, req *pb.ListTemplatesRequest) (*pb.ListTemplatesResponse, error) {
	return a.svc.ListTemplates(ctx, req)
}
func (a *localTemplateAdapter) DeleteTemplate(ctx context.Context, req *pb.DeleteTemplateRequest) (*emptypb.Empty, error) {
	return a.svc.DeleteTemplate(ctx, req)
}
func (a *localTemplateAdapter) InstantiateTemplate(ctx context.Context, req *pb.InstantiateTemplateRequest) (*pb.Change, error) {
	return a.svc.InstantiateTemplate(ctx, req)
}

func (a *remoteTemplateAdapter) CreateTemplate(ctx context.Context, req *pb.CreateTemplateRequest) (*pb.Template, error) {
	return a.client.CreateTemplate(ctx, req)
}
func (a *remoteTemplateAdapter) GetTemplate(ctx context.Context, req *pb.GetTemplateRequest) (*pb.Template, error) {
	return a.client.GetTemplate(ctx, req)
}
func (a *remoteTemplateAdapter) ListTemplates(ctx context.Context, req *pb.ListTemplatesRequest) (*pb.ListTemplatesResponse, error) {
	return a.client.ListTemplates(ctx, req)
}
func (a *remoteTemplateAdapter) DeleteTemplate(ctx context.Context, req *pb.DeleteTemplateRequest) (*emptypb.Empty, error) {
	return a.client.DeleteTemplate(ctx, req)
}
func (a *remoteTemplateAdapter) InstantiateTemplate(ctx context.Context, req *pb.InstantiateTemplateRequest) (*pb.Change, error) {
	return a.client.InstantiateTemplate(ctx, req)
}

// --- TargetService adapter --------------------------------------------------

// targetServiceAdapter unifies local and remote TargetService.
type targetServiceAdapter interface {
	AddTarget(ctx context.Context, req *pb.AddTargetRequest) (*pb.Target, error)
	RemoveTarget(ctx context.Context, req *pb.RemoveTargetRequest) (*emptypb.Empty, error)
	ListTargets(ctx context.Context, req *pb.ListTargetsRequest) (*pb.ListTargetsResponse, error)
	GetTarget(ctx context.Context, req *pb.GetTargetRequest) (*pb.Target, error)
	CheckTarget(ctx context.Context, req *pb.CheckTargetRequest) (*pb.CheckTargetResponse, error)
}

// GetTargetService returns a TargetService adapter for the active mode.
func (f *serviceFactory) GetTargetService() targetServiceAdapter {
	if f.mode == modeRemote {
		return &remoteTargetAdapter{client: f.remote.target}
	}
	return &localTargetAdapter{svc: f.local.target}
}

type localTargetAdapter struct {
	svc *grpc.TargetService
}

type remoteTargetAdapter struct {
	client pb.TargetServiceClient
}

func (a *localTargetAdapter) AddTarget(ctx context.Context, req *pb.AddTargetRequest) (*pb.Target, error) {
	return a.svc.AddTarget(ctx, req)
}
func (a *localTargetAdapter) RemoveTarget(ctx context.Context, req *pb.RemoveTargetRequest) (*emptypb.Empty, error) {
	return a.svc.RemoveTarget(ctx, req)
}
func (a *localTargetAdapter) ListTargets(ctx context.Context, req *pb.ListTargetsRequest) (*pb.ListTargetsResponse, error) {
	return a.svc.ListTargets(ctx, req)
}
func (a *localTargetAdapter) GetTarget(ctx context.Context, req *pb.GetTargetRequest) (*pb.Target, error) {
	return a.svc.GetTarget(ctx, req)
}
func (a *localTargetAdapter) CheckTarget(ctx context.Context, req *pb.CheckTargetRequest) (*pb.CheckTargetResponse, error) {
	return a.svc.CheckTarget(ctx, req)
}

func (a *remoteTargetAdapter) AddTarget(ctx context.Context, req *pb.AddTargetRequest) (*pb.Target, error) {
	return a.client.AddTarget(ctx, req)
}
func (a *remoteTargetAdapter) RemoveTarget(ctx context.Context, req *pb.RemoveTargetRequest) (*emptypb.Empty, error) {
	return a.client.RemoveTarget(ctx, req)
}
func (a *remoteTargetAdapter) ListTargets(ctx context.Context, req *pb.ListTargetsRequest) (*pb.ListTargetsResponse, error) {
	return a.client.ListTargets(ctx, req)
}
func (a *remoteTargetAdapter) GetTarget(ctx context.Context, req *pb.GetTargetRequest) (*pb.Target, error) {
	return a.client.GetTarget(ctx, req)
}
func (a *remoteTargetAdapter) CheckTarget(ctx context.Context, req *pb.CheckTargetRequest) (*pb.CheckTargetResponse, error) {
	return a.client.CheckTarget(ctx, req)
}

// --- AuditService adapter ---------------------------------------------------

// auditServiceAdapter unifies local and remote AuditService.
type auditServiceAdapter interface {
	GetAuditLog(ctx context.Context, req *pb.GetAuditLogRequest) (*pb.GetAuditLogResponse, error)
	ListAuditTraces(ctx context.Context, req *pb.ListAuditTracesRequest) (*pb.ListAuditTracesResponse, error)
	VerifyHashChain(ctx context.Context, req *pb.VerifyHashChainRequest) (*pb.VerifyHashChainResponse, error)
	GetRunReport(ctx context.Context, req *pb.GetRunReportRequest) (*pb.RunReport, error)
}

// GetAuditService returns an AuditService adapter for the active mode.
func (f *serviceFactory) GetAuditService() auditServiceAdapter {
	if f.mode == modeRemote {
		return &remoteAuditAdapter{client: f.remote.audit}
	}
	return &localAuditAdapter{svc: f.local.audit}
}

type localAuditAdapter struct {
	svc *grpc.AuditService
}

type remoteAuditAdapter struct {
	client pb.AuditServiceClient
}

func (a *localAuditAdapter) GetAuditLog(ctx context.Context, req *pb.GetAuditLogRequest) (*pb.GetAuditLogResponse, error) {
	return a.svc.GetAuditLog(ctx, req)
}
func (a *localAuditAdapter) ListAuditTraces(ctx context.Context, req *pb.ListAuditTracesRequest) (*pb.ListAuditTracesResponse, error) {
	return a.svc.ListAuditTraces(ctx, req)
}
func (a *localAuditAdapter) VerifyHashChain(ctx context.Context, req *pb.VerifyHashChainRequest) (*pb.VerifyHashChainResponse, error) {
	return a.svc.VerifyHashChain(ctx, req)
}
func (a *localAuditAdapter) GetRunReport(ctx context.Context, req *pb.GetRunReportRequest) (*pb.RunReport, error) {
	return a.svc.GetRunReport(ctx, req)
}

func (a *remoteAuditAdapter) GetAuditLog(ctx context.Context, req *pb.GetAuditLogRequest) (*pb.GetAuditLogResponse, error) {
	return a.client.GetAuditLog(ctx, req)
}
func (a *remoteAuditAdapter) ListAuditTraces(ctx context.Context, req *pb.ListAuditTracesRequest) (*pb.ListAuditTracesResponse, error) {
	return a.client.ListAuditTraces(ctx, req)
}
func (a *remoteAuditAdapter) VerifyHashChain(ctx context.Context, req *pb.VerifyHashChainRequest) (*pb.VerifyHashChainResponse, error) {
	return a.client.VerifyHashChain(ctx, req)
}
func (a *remoteAuditAdapter) GetRunReport(ctx context.Context, req *pb.GetRunReportRequest) (*pb.RunReport, error) {
	return a.client.GetRunReport(ctx, req)
}

// --- SystemService adapter --------------------------------------------------

// systemServiceAdapter unifies local and remote SystemService.
type systemServiceAdapter interface {
	GetVersion(ctx context.Context, req *emptypb.Empty) (*pb.VersionInfo, error)
	GetStatus(ctx context.Context, req *emptypb.Empty) (*pb.SystemStatus, error)
	GetConfig(ctx context.Context, req *pb.GetConfigRequest) (*pb.Config, error)
	RunDoctor(ctx context.Context, req *emptypb.Empty) (*pb.DoctorReport, error)
}

// GetSystemService returns a SystemService adapter for the active mode.
func (f *serviceFactory) GetSystemService() systemServiceAdapter {
	if f.mode == modeRemote {
		return &remoteSystemAdapter{client: f.remote.system}
	}
	return &localSystemAdapter{svc: f.local.system}
}

type localSystemAdapter struct {
	svc *grpc.SystemService
}

type remoteSystemAdapter struct {
	client pb.SystemServiceClient
}

func (a *localSystemAdapter) GetVersion(ctx context.Context, req *emptypb.Empty) (*pb.VersionInfo, error) {
	return a.svc.GetVersion(ctx, req)
}
func (a *localSystemAdapter) GetStatus(ctx context.Context, req *emptypb.Empty) (*pb.SystemStatus, error) {
	return a.svc.GetStatus(ctx, req)
}
func (a *localSystemAdapter) GetConfig(ctx context.Context, req *pb.GetConfigRequest) (*pb.Config, error) {
	return a.svc.GetConfig(ctx, req)
}
func (a *localSystemAdapter) RunDoctor(ctx context.Context, req *emptypb.Empty) (*pb.DoctorReport, error) {
	return a.svc.RunDoctor(ctx, req)
}

func (a *remoteSystemAdapter) GetVersion(ctx context.Context, req *emptypb.Empty) (*pb.VersionInfo, error) {
	return a.client.GetVersion(ctx, req)
}
func (a *remoteSystemAdapter) GetStatus(ctx context.Context, req *emptypb.Empty) (*pb.SystemStatus, error) {
	return a.client.GetStatus(ctx, req)
}
func (a *remoteSystemAdapter) GetConfig(ctx context.Context, req *pb.GetConfigRequest) (*pb.Config, error) {
	return a.client.GetConfig(ctx, req)
}
func (a *remoteSystemAdapter) RunDoctor(ctx context.Context, req *emptypb.Empty) (*pb.DoctorReport, error) {
	return a.client.RunDoctor(ctx, req)
}
