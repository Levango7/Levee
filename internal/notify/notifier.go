// Package notify provides a notification framework for the LEVEE closed-loop
// control system. It defines a channel-agnostic Notifier abstraction together
// with a NotificationManager that fans out messages to all registered channels
// at well-defined trigger points in the run lifecycle.
//
// The framework supports object grading (initiator / approver / oncall /
// subscriber / channel) so that callers can express who should receive a
// notification without coupling to any specific delivery mechanism. Concrete
// channels (webhook, email, slack, ...) implement the Notifier interface and
// are registered with the manager at start-up.
package notify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrEmptyEvent is returned when a message is constructed or sent without
	// an event identifier.
	ErrEmptyEvent = errors.New("notify: empty event")
	// ErrEmptyRunID is returned when a message is constructed or sent without
	// a run identifier.
	ErrEmptyRunID = errors.New("notify: empty run id")
	// ErrNoRecipients is returned when a message is sent without any
	// recipients.
	ErrNoRecipients = errors.New("notify: no recipients")
	// ErrNotifierNotFound is returned when an operation targets a notifier
	// that is not registered.
	ErrNotifierNotFound = errors.New("notify: notifier not found")
	// ErrNotifierExists is returned when attempting to register a notifier
	// whose name is already taken.
	ErrNotifierExists = errors.New("notify: notifier already registered")
	// ErrSendFailed is returned when one or more notifiers fail to send a
	// message. The wrapped error contains details about the failures.
	ErrSendFailed = errors.New("notify: send failed")
)

// --- Message level ----------------------------------------------------------

// MessageLevel is the severity level of a notification message.
type MessageLevel int

const (
	// LevelInfo is the default level for routine notifications.
	LevelInfo MessageLevel = iota
	// LevelWarning indicates something unexpected happened but the run can
	// continue.
	LevelWarning
	// LevelError indicates an error that prevented a step or run from
	// completing successfully.
	LevelError
	// LevelCritical indicates a severe condition that requires immediate human
	// intervention.
	LevelCritical
)

// String returns a human-readable representation of the message level.
func (l MessageLevel) String() string {
	switch l {
	case LevelInfo:
		return "info"
	case LevelWarning:
		return "warning"
	case LevelError:
		return "error"
	case LevelCritical:
		return "critical"
	default:
		return fmt.Sprintf("unknown(%d)", int(l))
	}
}

// --- Recipient --------------------------------------------------------------

// RecipientType classifies a recipient. It implements object grading so that
// callers can distinguish the role a recipient plays in the run lifecycle
// (initiator, approver, oncall, subscriber) or address a delivery channel
// directly (e.g. a Slack #channel).
type RecipientType int

const (
	// RecipientInitiator is the user who started the run.
	RecipientInitiator RecipientType = iota
	// RecipientApprover is a user who is asked to approve a gate or action.
	RecipientApprover
	// RecipientOncall is the user currently on call for the affected service.
	RecipientOncall
	// RecipientSubscriber is a user who explicitly subscribed to run
	// notifications.
	RecipientSubscriber
	// RecipientChannel addresses a delivery channel (e.g. a Slack channel)
	// rather than an individual user.
	RecipientChannel
)

// String returns a human-readable representation of the recipient type.
func (t RecipientType) String() string {
	switch t {
	case RecipientInitiator:
		return "initiator"
	case RecipientApprover:
		return "approver"
	case RecipientOncall:
		return "oncall"
	case RecipientSubscriber:
		return "subscriber"
	case RecipientChannel:
		return "channel"
	default:
		return fmt.Sprintf("unknown(%d)", int(t))
	}
}

// Recipient is a single notification recipient. The ID is interpreted in the
// context of Type: a username, an email address, a webhook URL, a Slack
// channel mention, etc. Name is a free-form display name used in rendered
// messages.
type Recipient struct {
	Type RecipientType
	ID   string
	Name string
}

// --- Trigger points ---------------------------------------------------------

// TriggerPoint identifies a notification trigger point in the closed-loop
// lifecycle. It is used as the Event field of a Message and as a stable
// identifier for filtering / routing.
type TriggerPoint string

const (
	TriggerRunStarted        TriggerPoint = "run_started"
	TriggerRunCompleted      TriggerPoint = "run_completed"
	TriggerRunFailed         TriggerPoint = "run_failed"
	TriggerStepStarted       TriggerPoint = "step_started"
	TriggerStepCompleted     TriggerPoint = "step_completed"
	TriggerStepFailed        TriggerPoint = "step_failed"
	TriggerGateFailed        TriggerPoint = "gate_failed"
	TriggerApprovalRequested TriggerPoint = "approval_requested"
	TriggerApprovalDecision  TriggerPoint = "approval_decision"
	TriggerRollbackTriggered TriggerPoint = "rollback_triggered"
	TriggerRollbackCompleted TriggerPoint = "rollback_completed"
	TriggerRollbackFailed    TriggerPoint = "rollback_failed"
	TriggerPaused            TriggerPoint = "paused"
	TriggerResumed           TriggerPoint = "resumed"
)

// --- Message ---------------------------------------------------------------

// Message is a single notification. It carries the triggering event, the
// associated run, the severity, the rendered title/body, the recipient list
// and arbitrary metadata. Concrete Notifier implementations are responsible
// for formatting and delivering the message.
type Message struct {
	ID         string            // unique message id (auto-generated by NewMessage)
	Event      string            // trigger event, e.g. "run_started"
	RunID      string            // associated run id
	Level      MessageLevel      // severity
	Title      string            // short, human-readable title
	Body       string            // detailed body
	Recipients []Recipient       // recipients
	Metadata   map[string]string // arbitrary metadata
	Timestamp  time.Time         // when the message was created
}

// --- Notifier interface ----------------------------------------------------

// Notifier is the abstraction implemented by every concrete delivery channel
// (webhook, email, slack, ...). The MVP ships only the webhook channel
// (T052), but the framework is channel-agnostic.
type Notifier interface {
	// Name returns the channel's unique name. It is used as the registration
	// key with NotificationManager.
	Name() string
	// Send delivers a single message. Implementations must honour ctx
	// cancellation and timeouts.
	Send(ctx context.Context, msg Message) error
}

// --- NotificationManager ---------------------------------------------------

// NotificationManager owns the set of registered Notifier channels and fans
// out messages to all of them. It is safe for concurrent use.
type NotificationManager struct {
	notifiers map[string]Notifier
	mu        sync.RWMutex
}

// NewNotificationManager returns an empty NotificationManager ready to have
// notifiers registered.
func NewNotificationManager() *NotificationManager {
	return &NotificationManager{
		notifiers: make(map[string]Notifier),
	}
}

// Register adds a notifier to the manager. It returns ErrNotifierExists if a
// notifier with the same name is already registered. The notifier must have a
// non-empty Name().
func (m *NotificationManager) Register(n Notifier) error {
	if n == nil {
		return fmt.Errorf("notify: register: %w", ErrNotifierNotFound)
	}
	name := n.Name()
	if name == "" {
		return fmt.Errorf("notify: register: %w", ErrNotifierNotFound)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.notifiers[name]; exists {
		return fmt.Errorf("notify: register %q: %w", name, ErrNotifierExists)
	}
	m.notifiers[name] = n
	log.Info("notify: registered notifier", "name", name)
	return nil
}

// Unregister removes a notifier by name. It returns ErrNotifierNotFound if no
// notifier with the given name is registered.
func (m *NotificationManager) Unregister(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.notifiers[name]; !exists {
		return fmt.Errorf("notify: unregister %q: %w", name, ErrNotifierNotFound)
	}
	delete(m.notifiers, name)
	log.Info("notify: unregistered notifier", "name", name)
	return nil
}

// Has reports whether a notifier with the given name is currently registered.
func (m *NotificationManager) Has(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.notifiers[name]
	return ok
}

// Names returns the names of all registered notifiers in lexical order. The
// returned slice is a copy and may be safely modified by the caller.
func (m *NotificationManager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.notifiers))
	for name := range m.notifiers {
		names = append(names, name)
	}
	// Sort for stable output (helps tests and logs).
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return names
}

// validateMessage performs the common pre-send checks shared by Notify and
// NotifyAsync. It returns a wrapped sentinel error on failure.
func validateMessage(msg Message) error {
	if msg.Event == "" {
		return fmt.Errorf("notify: %w", ErrEmptyEvent)
	}
	if msg.RunID == "" {
		return fmt.Errorf("notify: %w", ErrEmptyRunID)
	}
	if len(msg.Recipients) == 0 {
		return fmt.Errorf("notify: %w", ErrNoRecipients)
	}
	return nil
}

// Notify synchronously delivers msg to every registered notifier. A failure
// in one notifier does not stop delivery to the others; instead all failures
// are aggregated and returned as a single error wrapping ErrSendFailed. If
// there are no registered notifiers the call is a no-op and returns nil.
//
// The message is validated before delivery: an empty Event, RunID or
// Recipients list causes an immediate error without invoking any notifier.
func (m *NotificationManager) Notify(ctx context.Context, msg Message) error {
	if err := validateMessage(msg); err != nil {
		return err
	}

	m.mu.RLock()
	snapshot := make([]Notifier, 0, len(m.notifiers))
	for _, n := range m.notifiers {
		snapshot = append(snapshot, n)
	}
	m.mu.RUnlock()

	if len(snapshot) == 0 {
		return nil
	}

	var (
		errs   []error
		errsMu sync.Mutex
		wg     sync.WaitGroup
	)

	for _, n := range snapshot {
		wg.Add(1)
		go func(n Notifier) {
			defer wg.Done()
			if err := n.Send(ctx, msg); err != nil {
				errsMu.Lock()
				errs = append(errs, fmt.Errorf("notify: %s: %w", n.Name(), err))
				errsMu.Unlock()
				log.Warn("notify: send failed",
					"notifier", n.Name(),
					"event", msg.Event,
					"run_id", msg.RunID,
					"err", err)
			}
		}(n)
	}
	wg.Wait()

	if len(errs) > 0 {
		// Wrap the first error with ErrSendFailed and include the count in
		// the message. Callers can use errors.Is(err, ErrSendFailed) to
		// detect partial failures.
		return fmt.Errorf("notify: %d of %d sends failed: %w (first: %v)",
			len(errs), len(snapshot), ErrSendFailed, errs[0])
	}
	return nil
}

// NotifyAsync delivers msg in a background goroutine and returns immediately.
// It never blocks the caller and always returns a nil error; delivery
// failures are logged but not surfaced. Use Notify when you need to observe
// failures synchronously.
//
// The provided context is used only for the validation step; the background
// goroutine uses context.WithoutCancel(ctx) (or context.Background() on
// older Go versions) so that cancellation of the caller's context does not
// abort in-flight deliveries. If you need cancellation, use Notify with a
// deadline-bearing context.
func (m *NotificationManager) NotifyAsync(ctx context.Context, msg Message) {
	go func() {
		// Detach from the caller's context so that cancelling ctx after
		// NotifyAsync returns does not interrupt delivery.
		bg := context.Background()
		if err := m.Notify(bg, msg); err != nil {
			log.Error("notify: async send failed",
				"event", msg.Event,
				"run_id", msg.RunID,
				"err", err)
		}
	}()
}

// --- Constructors ----------------------------------------------------------

// generateID returns a random 16-byte hex string suitable for use as a
// Message.ID. If the crypto RNG fails it falls back to a timestamp-based id
// so that construction never fails.
func generateID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback: never fail construction.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// NewMessage constructs a Message with a generated ID and the current
// timestamp. Recipients and Metadata are left nil; callers can append to
// them after construction. The event and runID are stored as-is; validation
// happens at send time.
func NewMessage(event, runID string, level MessageLevel, title, body string) Message {
	return Message{
		ID:        generateID(),
		Event:     event,
		RunID:     runID,
		Level:     level,
		Title:     title,
		Body:      body,
		Timestamp: time.Now(),
	}
}

// NewRecipient constructs a Recipient with the given type, id and name.
func NewRecipient(rtype RecipientType, id, name string) Recipient {
	return Recipient{Type: rtype, ID: id, Name: name}
}

// Initiator is a convenience constructor for a RecipientInitiator.
func Initiator(id, name string) Recipient {
	return NewRecipient(RecipientInitiator, id, name)
}

// Approver is a convenience constructor for a RecipientApprover.
func Approver(id, name string) Recipient {
	return NewRecipient(RecipientApprover, id, name)
}

// Oncall is a convenience constructor for a RecipientOncall.
func Oncall(id, name string) Recipient {
	return NewRecipient(RecipientOncall, id, name)
}

// Subscriber is a convenience constructor for a RecipientSubscriber.
func Subscriber(id, name string) Recipient {
	return NewRecipient(RecipientSubscriber, id, name)
}

// Channel is a convenience constructor for a RecipientChannel.
func Channel(id, name string) Recipient {
	return NewRecipient(RecipientChannel, id, name)
}
