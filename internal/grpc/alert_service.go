
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
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/alert"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/log"

	"google.golang.org/grpc/codes"
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

// --- ReceiveAlert ----------------------------------------------------------

// ReceiveAlert ingests a single alert, stores it in the in-memory ring and
// broadcasts it to all active subscribers. The alert id is taken from the
// request when present, otherwise a fingerprint is generated.
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
	if a.Status == alert.StatusFiring && a.EndsAt.IsZero() {
		// leave firing
	}

	if err := a.Validate(); err != nil {
		return &pb.AlertResponse{
			Status: "invalid",
			Id:     a.ID,
			Reason: err.Error(),
		}, nil
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

	return &pb.AlertStatus{
		Id:       a.ID,
		Status:   a.Status.String(),
		StartsAt: a.StartsAt.Unix(),
		EndsAt:   a.EndsAt.Unix(),
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

	for {
		select {
		case <-ctx.Done():
			return nil
		case a := <-ch:
			if a == nil {
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