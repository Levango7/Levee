// service_factory.go implements the dual-mode Service factory and the
// adapter interfaces behind the --local / --remote flags:
//
//   - modeLocal:  in-process service implementations backed directly by a
//                 state.Store. This is the default and keeps the CLI a single
//                 zero-dependency binary for development and air-gapped use.
//   - modeRemote: thin adapters around the generated gRPC clients, forwarding
//                 every call to a remote LEVEE server over the network.
//
// IMPORTANT - what this file is TODAY, stated plainly:
//
//   - LIVE callers: `agent list / show / remove` do NOT go through here (they
//     call newGRPCClient directly; see cmd_agent_remote.go), and neither do
//     plan/apply. The commands wired through this factory are `version` and
//     `system version`, in both modes: local answers from the build variables,
//     remote asks the server. That is the first case where "one place decides
//     which side answers" is actually exercised in production, and it is what
//     keeps the two modes from growing separate renderers.
//   - The remaining read-only families were each settled by a per-command
//     decision rather than by plumbing: where local output exceeded the wire,
//     the proto grew (`template list` needed the file library on the serve side
//     plus id/tags; `target list` needed created_at/last_checked_at/tenant_id
//     and group_name; `audit verify` needed every failure, not just the first;
//     `template show` needed parameters). `system status` took the other
//     branch — "remote shows less": GetStatus authenticates but performs no
//     policy check, so its payload stays runtime counts/uptime/store type/
//     warnings, and config_path plus the db_* fields remain local-only. The CLI
//     names those fields explicitly so a reduced remote answer cannot be
//     mistaken for a local one (see remoteLocalOnlyStatusFields in
//     cmd_system.go).
//   - The change service's local build delegates to newCLIChangeService, which
//     carries the change calendar gates, the credential resolver and the --as
//     authorizer; a drift guard in grpc_client_test.go asserts that stays true.
//
// Services are built on demand (see localServices): a local build no longer
// requires a store, so asking for a version cannot create a database.
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

// localServices holds the local-mode dependencies and builds the in-process
// service implementations on demand.
//
// On demand, not at construction: a factory built eagerly required a store for
// every service, so wiring a command that needs no store at all (`system
// version` reads build variables) would have opened a database as a side
// effect. A store-less request now fails only where it would be a lie — at the
// service that actually reads the store — instead of at the factory door.
type localServices struct {
	deps localDeps
}

// errLocalStoreRequired is returned for a service that cannot work without a
// store. Callers hand it up unchanged so the message names the missing piece.
var errLocalStoreRequired = errors.New("service factory: local mode needs a store for this service")

// change builds the change service through the SAME constructor production
// uses (newCLIChangeService): the change calendar gates, the credential
// resolver and the --as authorizer all live there, and this factory used to
// build a bare service without any of them - the drift that made this file's
// local path diverge from what every command actually runs.
func (ls *localServices) change() (*grpc.ChangeService, error) {
	if ls.deps.store == nil {
		return nil, errLocalStoreRequired
	}
	svc, err := newCLIChangeService(ls.deps.store)
	if err != nil {
		return nil, fmt.Errorf("build local change service: %w", err)
	}
	return svc, nil
}

// template works with either the file library or the store: the service prefers
// the library when it is set and falls back to stored templates otherwise.
func (ls *localServices) template() (*grpc.TemplateService, error) {
	if ls.deps.store == nil && ls.deps.templateLib == nil {
		return nil, errLocalStoreRequired
	}
	return grpc.NewTemplateService(ls.deps.store, ls.deps.templateLib), nil
}

func (ls *localServices) target() (*grpc.TargetService, error) {
	if ls.deps.store == nil {
		return nil, errLocalStoreRequired
	}
	return grpc.NewTargetService(ls.deps.store, ls.deps.channelFactory), nil
}

func (ls *localServices) audit() (*grpc.AuditService, error) {
	if ls.deps.store == nil {
		return nil, errLocalStoreRequired
	}
	return grpc.NewAuditService(ls.deps.store), nil
}

// system needs no store: GetVersion is answered from the build variables baked
// into this binary, and the store is only consulted by GetStatus.
func (ls *localServices) system() *grpc.SystemService {
	cfg := ls.deps.config
	if cfg == nil {
		// Without a config the service still answers GetVersion; the rest of
		// its surface degrades to "store not configured" instead of panicking.
		cfg = &config.Config{}
	}
	return grpc.NewSystemService(
		ls.deps.store, cfg, ls.deps.configPath,
		version, commitHash, buildTime, goVersion, time.Now(),
	)
}

// localDeps carries the optional internal dependencies that the local service
// implementations can use. Remote mode ignores this entirely. Every field is
// optional; nil falls back to a store-only / reduced-functionality code path.
type localDeps struct {
	store          state.Store
	templateLib    *template.TemplateLibrary
	channelFactory channel.ChannelFactory
	config         *config.Config
	configPath     string
	// engine and approval were removed: the change service builds its own
	// through newCLIChangeService (the production constructor), and the
	// factory's tests never supplied either.
}

// factoryOption configures newServiceFactory.
type factoryOption func(*factoryConfig)

// factoryConfig is the internal configuration bag.
type factoryConfig struct {
	deps *localDeps
}

// WithFactoryDeps supplies the local-mode dependencies. Ignored in remote mode
// except for store, which is unused remotely.
func WithFactoryDeps(deps *localDeps) factoryOption {
	return func(c *factoryConfig) {
		c.deps = deps
	}
}

// newServiceFactory builds a serviceFactory for the requested mode.
//
//   - modeLocal:  builds in-process services from the supplied deps. The store
//     is required only by the services that read it (see localServices), so a
//     caller that needs nothing but build information does not open a database.
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
		deps := localDeps{}
		if cfg.deps != nil {
			deps = *cfg.deps
		}
		f.local = &localServices{deps: deps}

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
func (f *serviceFactory) GetChangeService() (changeServiceAdapter, error) {
	if f.mode == modeRemote {
		return &remoteChangeAdapter{client: f.remote.change}, nil
	}
	svc, err := f.local.change()
	if err != nil {
		return nil, err
	}
	return &localChangeAdapter{svc: svc}, nil
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
func (f *serviceFactory) GetTemplateService() (templateServiceAdapter, error) {
	if f.mode == modeRemote {
		return &remoteTemplateAdapter{client: f.remote.template}, nil
	}
	svc, err := f.local.template()
	if err != nil {
		return nil, err
	}
	return &localTemplateAdapter{svc: svc}, nil
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
func (f *serviceFactory) GetTargetService() (targetServiceAdapter, error) {
	if f.mode == modeRemote {
		return &remoteTargetAdapter{client: f.remote.target}, nil
	}
	svc, err := f.local.target()
	if err != nil {
		return nil, err
	}
	return &localTargetAdapter{svc: svc}, nil
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
	GetAuditTrace(ctx context.Context, req *pb.GetAuditTraceRequest) (*pb.TraceEntry, error)
}

// GetAuditService returns an AuditService adapter for the active mode.
func (f *serviceFactory) GetAuditService() (auditServiceAdapter, error) {
	if f.mode == modeRemote {
		return &remoteAuditAdapter{client: f.remote.audit}, nil
	}
	svc, err := f.local.audit()
	if err != nil {
		return nil, err
	}
	return &localAuditAdapter{svc: svc}, nil
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
func (a *localAuditAdapter) GetAuditTrace(ctx context.Context, req *pb.GetAuditTraceRequest) (*pb.TraceEntry, error) {
	return a.svc.GetAuditTrace(ctx, req)
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
func (a *remoteAuditAdapter) GetAuditTrace(ctx context.Context, req *pb.GetAuditTraceRequest) (*pb.TraceEntry, error) {
	return a.client.GetAuditTrace(ctx, req)
}

// --- SystemService adapter --------------------------------------------------

// systemServiceAdapter unifies local and remote SystemService.
type systemServiceAdapter interface {
	GetVersion(ctx context.Context, req *emptypb.Empty) (*pb.VersionInfo, error)
	GetStatus(ctx context.Context, req *emptypb.Empty) (*pb.SystemStatus, error)
	GetConfig(ctx context.Context, req *pb.GetConfigRequest) (*pb.Config, error)
	RunDoctor(ctx context.Context, req *emptypb.Empty) (*pb.DoctorReport, error)
}

// GetSystemService returns a SystemService adapter for the active mode. Local
// mode never needs a store here: GetVersion answers from the build variables,
// and the store-reading methods degrade to "store not configured".
func (f *serviceFactory) GetSystemService() (systemServiceAdapter, error) {
	if f.mode == modeRemote {
		return &remoteSystemAdapter{client: f.remote.system}, nil
	}
	return &localSystemAdapter{svc: f.local.system()}, nil
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
