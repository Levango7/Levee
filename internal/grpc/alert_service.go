// alert_service.go implements pb.AlertServiceServer, the gRPC service that
// ingests alerts, exposes alert status and streams alerts to subscribers.
//
// The service is intentionally self-contained: it keeps a small in-memory
// store of recently received alerts (bounded by maxRecentAlerts) and a
// broadcast channel for subscribers. When an *alert.AlertGateway is
// supplied it is used as an additional source of recent alerts (via
// RecentAlerts); when it is nil the service still functions in
// stand-alone mode.
//
// All errors are mapped to gRPC codes via the status package. Nil
// dependencies degrade to codes.Unimplemented rather than panicking.

package grpc

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/alert"
	"github.com/nexus/levee/internal/authz"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/log"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// maxRecentAlerts caps the in-memory alert ring kept by AlertService.
const maxRecentAlerts = 256

// AlertService implements pb.AlertServiceServer. It is safe for concurrent
// use and immutable after construction (the dependencies captured in the
// struct are never reassigned).
type AlertService struct {
	pb.UnimplementedAlertServiceServer

	// gateway is an optional source of recent alerts. May be nil.
	gateway *alert.AlertGateway

	// sources is the optional ingress-identity registry (alert-sources.yaml).
	// When configured, ReceiveAlert requires a per-source credential and the
	// alert's environment comes from the registration; when nil/empty the
	// historical authentication-only behaviour is kept.
	sources *alert.SourceRegistry

	// authz is the optional policy authorizer. It judges the READ side
	// (GetAlertStatus / SubscribeAlerts) against the alert's owned environment;
	// nil means no policy configured and reads are admitted.
	authz *authz.Authorizer

	// log is the structured logger. When nil the package-level singleton
	// from internal/log is used.
	log *slog.Logger

	// mu guards alerts and subscribers.
	mu sync.RWMutex

	// alerts is the bounded ring of recently received alerts, keyed by id.
	alerts   map[string]*alert.Alert
	alertSeq []*alert.Alert // insertion order, oldest first

	// subscribers is the set of active SubscribeAlerts goroutines. Each
	// subscriber owns a buffered channel; ReceiveAlert pushes a copy of
	// every accepted alert to every subscriber. Subscribers remove
	// themselves on stream close.
	subscribers map[int64]chan *alert.Alert
	nextSubID   int64
}

// NewAlertService constructs an AlertService. Both gateway and logger are
// optional; passing nil for either is supported and the service degrades
// gracefully.
func NewAlertService(gateway *alert.AlertGateway, lg *slog.Logger) *AlertService {
	if lg == nil {
		lg = log.With("component", "alert_service")
	}
	return &AlertService{
		gateway:     gateway,
		log:         lg,
		alerts:      make(map[string]*alert.Alert),
		subscribers: make(map[int64]chan *alert.Alert),
	}
}

// WithAlertSources installs the ingress-identity registry. When it has at
// least one source, ReceiveAlert demands a matching per-source credential and
// attributes the alert to that source's environment. A nil or empty registry
// leaves ingress authentication-only (the pre-existing behaviour).
func (s *AlertService) WithAlertSources(reg *alert.SourceRegistry) *AlertService {
	s.sources = reg
	return s
}

// WithAuthorizer installs the policy authorizer consulted by GetAlertStatus
// and SubscribeAlerts. Nil means no policy configured; reads are then admitted
// exactly as before.
func (s *AlertService) WithAuthorizer(a *authz.Authorizer) *AlertService {
	s.authz = a
	return s
}

// --- ReceiveAlert ----------------------------------------------------------

// ReceiveAlert ingests a single alert, stores it in the in-memory ring and
// broadcasts it to all active subscribers. The alert id is taken from the
// request when present, otherwise a fingerprint is generated.
//
// Why an alert is NOT judged against the team × environment matrix directly:
// AlertMessage carries id / source / severity / ends_at only — no team, no
// environment, and no target whose labels could be resolved to one. There is
// nothing for an environment-keyed policy to decide against, and two tempting
// shortcuts both lie: judging it in permission.default_env would grant or refuse
// a monitoring feed by whoever happens to own the default environment, and
// adding an `alert` action token would fail open on every existing matrix (an
// unlisted action has no grant, and this deployment posture is
// no-matrix-means-not-enforced, see authz.Authorizer.Decide).
//
// What fills the gap is ownership from configuration, not from the payload. An
// alert source is a machine that owns an environment (internal/alert/sources.go,
// alert-sources.yaml): the credential proves WHICH source is speaking, and the
// registration says what its alerts belong to. When sources are configured,
// ReceiveAlert requires that credential and stamps the alert with the source's
// environment; the READ side (GetAlertStatus / SubscribeAlerts) then asks the
// matrix for `view` on that environment. A caller cannot self-declare either
// half. Unconfigured deployments keep the historical authentication-only
// ingress, announced at startup rather than decided silently per request.
func (s *AlertService) ReceiveAlert(ctx context.Context, req *pb.AlertMessage) (*pb.AlertResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}
	if strings.TrimSpace(req.GetSource()) == "" {
		return nil, status.Error(codes.InvalidArgument, "source is required")
	}
	if strings.TrimSpace(req.GetTitle()) == "" {
		return nil, status.Error(codes.InvalidArgument, "title is required")
	}

	a := pbAlertToAlert(req)
	if a.ID == "" {
		a.ID = a.GenerateFingerprint()
	}
	if a.Fingerprint == "" {
		a.Fingerprint = a.GenerateFingerprint()
	}
	if a.StartsAt.IsZero() {
		a.StartsAt = time.Now()
	}
	// firing alert already has an end time; leave it as-is.
	if a.Status == alert.StatusFiring && !a.EndsAt.IsZero() { //nolint:staticcheck // SA4017 false positive: negation clearly uses IsZero result
		// no-op: keep the existing EndsAt
	}

	if err := a.Validate(); err != nil {
		return &pb.AlertResponse{
			Status: "invalid",
			Id:     a.ID,
			Reason: err.Error(),
		}, nil
	}

	// Ingress identity: when sources are registered, the caller must present a
	// registered source name and its secret, and the alert belongs to that
	// source's environment. This is deliberately before recordAlert/broadcast —
	// an unauthenticated alert must leave no trace in the ring or the stream.
	if s.sources.Configured() {
		src, err := s.authenticateSource(ctx)
		if err != nil {
			return nil, err
		}
		a.Environment = src.Environment
	}

	s.recordAlert(a)
	s.broadcast(a)

	return &pb.AlertResponse{
		Status: "accepted",
		Id:     a.ID,
	}, nil
}

// --- GetAlertStatus --------------------------------------------------------

// GetAlertStatus returns the current status of an alert by id. It first
// checks the in-memory ring, then falls back to the gateway's RecentAlerts
// when configured. Returns codes.NotFound when the alert is unknown.
func (s *AlertService) GetAlertStatus(ctx context.Context, req *pb.GetAlertStatusRequest) (*pb.AlertStatus, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}
	if strings.TrimSpace(req.GetId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}

	a := s.lookupAlert(req.GetId())
	if a == nil {
		return nil, status.Errorf(codes.NotFound, "alert %q not found", req.GetId())
	}

	// Visibility is judged on the alert's OWNED environment, never on one the
	// caller supplies: the ownership comes from the registered source (see
	// internal/alert/sources.go), so a caller cannot widen its own view by
	// naming an alert it happens to know the id of.
	if err := authorizeResourceRead(ctx, s.authz, a.Environment, "GetAlertStatus"); err != nil {
		return nil, err
	}

	// Zero (unset) timestamps are emitted as 0 rather than the huge
	// negative Unix value time.Time.Zero().Unix() produces.
	startsAt := int64(0)
	if !a.StartsAt.IsZero() {
		startsAt = a.StartsAt.Unix()
	}
	endsAt := int64(0)
	if !a.EndsAt.IsZero() {
		endsAt = a.EndsAt.Unix()
	}

	return &pb.AlertStatus{
		Id:       a.ID,
		Status:   a.Status.String(),
		StartsAt: startsAt,
		EndsAt:   endsAt,
		Severity: a.Severity.String(),
	}, nil
}

// --- SubscribeAlerts -------------------------------------------------------

// SubscribeAlerts opens a server-stream of alerts matching the supplied
// filter. The stream stays open until the client disconnects or the stream
// context is cancelled. A nil request is treated as an empty filter.
func (s *AlertService) SubscribeAlerts(req *pb.SubscribeRequest, stream pb.AlertService_SubscribeAlertsServer) error {
	if req == nil {
		req = &pb.SubscribeRequest{}
	}
	ctx := stream.Context()

	// Register a subscriber. The channel is buffered so a slow client
	// does not block ReceiveAlert; when the buffer fills the alert is
	// dropped (best-effort delivery).
	ch := make(chan *alert.Alert, 32)
	subID := s.addSubscriber(ch)
	defer s.removeSubscriber(subID)

	// Visibility narrows the stream rather than refusing it: a caller entitled
	// to one environment must still receive that environment's alerts, and an
	// empty stream is the honest answer for a caller entitled to none (the same
	// rule ListChanges and the other list endpoints follow).
	visible := resourceVisibility(ctx, s.authz)

	for {
		select {
		case <-ctx.Done():
			return nil
		case a := <-ch:
			if a == nil {
				continue
			}
			if visible != nil && !visible(a.Environment) {
				continue
			}
			if !matchAlertFilter(a, req) {
				continue
			}
			if err := stream.Send(alertToPB(a)); err != nil {
				return err
			}
		}
	}
}

// --- internal helpers ------------------------------------------------------

// authenticateSource resolves the per-source ingress credential presented on
// the context. It returns an Unauthenticated status (not PermissionDenied):
// the caller failed to prove WHICH source it is, which is an authentication
// question, and the subject is a machine rather than a team the matrix could
// judge.
func (s *AlertService) authenticateSource(ctx context.Context) (alert.Source, error) {
	name, token := alertSourceCredential(ctx)
	if name == "" {
		return alert.Source{}, status.Errorf(codes.Unauthenticated,
			"ReceiveAlert requires an alert source credential (metadata %q + %q); this deployment registered alert sources, so an alert without one cannot be attributed to an environment",
			alert.SourceMetadataKey, alert.TokenMetadataKey)
	}
	src, ok := s.sources.Lookup(name)
	if !ok {
		return alert.Source{}, status.Errorf(codes.Unauthenticated,
			"alert source %q is not registered", name)
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(src.Secret)) != 1 {
		return alert.Source{}, status.Errorf(codes.Unauthenticated,
			"alert source %q presented an invalid credential", name)
	}
	return src, nil
}

// alertSourceCredential reads the source name and token from the context
// metadata. Both the incoming (real gRPC) and outgoing (the REST gateway calls
// the service in-process with outgoing metadata) directions are checked, so the
// same registry guards either transport without the service knowing which one
// it is behind.
func alertSourceCredential(ctx context.Context) (name, token string) {
	pick := func(md metadata.MD) (string, string) {
		return firstMetadataValue(md, alert.SourceMetadataKey), firstMetadataValue(md, alert.TokenMetadataKey)
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if n, t := pick(md); n != "" || t != "" {
			return n, t
		}
	}
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		return pick(md)
	}
	return "", ""
}

func firstMetadataValue(md metadata.MD, key string) string {
	vals := md.Get(key)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

// recordAlert stores a in the in-memory ring, evicting the oldest entry
// when the ring is full.
func (s *AlertService) recordAlert(a *alert.Alert) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.alerts[a.ID]; !exists {
		s.alertSeq = append(s.alertSeq, a)
		if len(s.alertSeq) > maxRecentAlerts {
			oldest := s.alertSeq[0]
			s.alertSeq = s.alertSeq[1:]
			delete(s.alerts, oldest.ID)
		}
	} else {
		// Update in place; sequence order unchanged.
		for i, e := range s.alertSeq {
			if e.ID == a.ID {
				s.alertSeq[i] = a
				break
			}
		}
	}
	s.alerts[a.ID] = a
}

// lookupAlert returns the alert with the given id, or nil when not found.
// It checks the in-memory ring first, then the gateway's RecentAlerts.
func (s *AlertService) lookupAlert(id string) *alert.Alert {
	s.mu.RLock()
	if a, ok := s.alerts[id]; ok {
		s.mu.RUnlock()
		return a
	}
	s.mu.RUnlock()

	if s.gateway != nil {
		for _, a := range s.gateway.RecentAlerts() {
			if a != nil && a.ID == id {
				return a
			}
		}
	}
	return nil
}

// broadcast sends a to every active subscriber. Slow subscribers (full
// channel) silently drop the alert to protect the hot path.
func (s *AlertService) broadcast(a *alert.Alert) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ch := range s.subscribers {
		select {
		case ch <- a:
		default:
			// channel full; drop to avoid blocking the producer.
		}
	}
}

// addSubscriber registers ch and returns its id.
func (s *AlertService) addSubscriber(ch chan *alert.Alert) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSubID++
	id := s.nextSubID
	s.subscribers[id] = ch
	return id
}

// removeSubscriber removes the subscriber with the given id.
func (s *AlertService) removeSubscriber(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.subscribers[id]; ok {
		close(ch)
		delete(s.subscribers, id)
	}
}

// SubscriberCount returns the number of active SubscribeAlerts subscribers.
// It is safe for concurrent use and is intended for observability and test
// synchronization (broadcast is best-effort and only reaches subscribers
// that have completed registration).
func (s *AlertService) SubscriberCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.subscribers)
}

// matchAlertFilter reports whether a matches the subscribe filter.
func matchAlertFilter(a *alert.Alert, req *pb.SubscribeRequest) bool {
	if req == nil {
		return true
	}
	if sev := strings.TrimSpace(req.GetSeverity()); sev != "" && !strings.EqualFold(a.Severity.String(), sev) {
		return false
	}
	if src := strings.TrimSpace(req.GetSource()); src != "" && !strings.EqualFold(a.Source, src) {
		return false
	}
	return true
}

// --- conversion helpers ----------------------------------------------------

// pbAlertToAlert converts a pb.AlertMessage to an alert.Alert. The mapping
// is best-effort: unknown severity strings fall back to SeverityInfo.
func pbAlertToAlert(req *pb.AlertMessage) *alert.Alert {
	a := &alert.Alert{
		ID:          req.GetId(),
		Source:      req.GetSource(),
		Title:       req.GetTitle(),
		Description: req.GetDescription(),
		Labels:      req.Labels,
		Fingerprint: req.GetFingerprint(),
	}
	if sev, err := alert.ParseSeverity(req.GetSeverity()); err == nil {
		a.Severity = sev
	} else {
		a.Severity = alert.SeverityInfo
	}
	if st, err := alert.ParseAlertStatus(req.GetStatus()); err == nil {
		a.Status = st
	} else {
		a.Status = alert.StatusFiring
	}
	if req.GetStartsAt() > 0 {
		a.StartsAt = time.Unix(req.GetStartsAt(), 0)
	}
	if req.GetEndsAt() > 0 {
		a.EndsAt = time.Unix(req.GetEndsAt(), 0)
	}
	return a
}

// alertToPB converts an alert.Alert to a pb.AlertMessage.
func alertToPB(a *alert.Alert) *pb.AlertMessage {
	if a == nil {
		return nil
	}
	return &pb.AlertMessage{
		Id:          a.ID,
		Source:      a.Source,
		Severity:    a.Severity.String(),
		Target:      a.Labels["instance"],
		Title:       a.Title,
		Description: a.Description,
		Labels:      a.Labels,
		Fingerprint: a.Fingerprint,
		StartsAt:    a.StartsAt.Unix(),
		EndsAt:      a.EndsAt.Unix(),
		Status:      a.Status.String(),
	}
}
