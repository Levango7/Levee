// Package chatops implements the ChatOps layer for LEVEE. It provides a
// platform-agnostic Bot abstraction together with a BotManager that owns the
// set of registered platform bots (Feishu / DingTalk / Slack / ...), fans out
// LEVEE lifecycle events to them, and routes user messages back into the
// approval / state services through a small command dispatcher.
//
// The framework is intentionally minimal: concrete platform adapters live in
// sibling files (feishu.go, dingtalk.go, slack.go) and implement the Bot
// interface defined here. The BotManager is safe for concurrent use.
package chatops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrBotNotFound is returned when an operation targets a bot that is not
	// registered with the BotManager.
	ErrBotNotFound = errors.New("chatops: bot not found")
	// ErrBotExists is returned when attempting to register a bot whose name
	// is already taken.
	ErrBotExists = errors.New("chatops: bot already registered")
	// ErrUnknownPlatform is returned when a platform identifier does not map
	// to a known adapter.
	ErrUnknownPlatform = errors.New("chatops: unknown platform")
	// ErrEmptyCommand is returned when a user message does not contain a
	// parseable command.
	ErrEmptyCommand = errors.New("chatops: empty command")
	// ErrUnknownCommand is returned when a command verb is not recognised.
	ErrUnknownCommand = errors.New("chatops: unknown command")
	// ErrCommandFailed is returned when a command handler returns an error.
	ErrCommandFailed = errors.New("chatops: command failed")
	// ErrBotClosed is returned when an operation is attempted on a bot whose
	// event channel has been closed.
	ErrBotClosed = errors.New("chatops: bot closed")
)

// --- Platform ---------------------------------------------------------------

// Platform identifies a ChatOps platform adapter.
type Platform string

const (
	// PlatformFeishu is the Feishu (Lark) adapter.
	PlatformFeishu Platform = "feishu"
	// PlatformDingtalk is the DingTalk adapter.
	PlatformDingtalk Platform = "dingtalk"
	// PlatformSlack is the Slack adapter.
	PlatformSlack Platform = "slack"
)

// ParsePlatform normalises a human-friendly platform name to a Platform
// constant. It is case-insensitive and accepts common aliases. Unknown
// values yield ErrUnknownPlatform.
func ParsePlatform(s string) (Platform, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "feishu", "lark":
		return PlatformFeishu, nil
	case "dingtalk", "dingding", "dd":
		return PlatformDingtalk, nil
	case "slack":
		return PlatformSlack, nil
	default:
		return "", fmt.Errorf("chatops: parse platform %q: %w", s, ErrUnknownPlatform)
	}
}

// --- Event ------------------------------------------------------------------

// EventType is the kind of LEVEE lifecycle event delivered to a bot. It
// mirrors a subset of notify.TriggerPoint so that the ChatOps layer can stay
// decoupled from the notify package.
type EventType string

const (
	EventStateChange       EventType = "state_change"
	EventApprovalRequested EventType = "approval_requested"
	EventApprovalDecision  EventType = "approval_decision"
	EventGateResult        EventType = "gate_result"
	EventRunStarted        EventType = "run_started"
	EventRunCompleted      EventType = "run_completed"
	EventRunFailed         EventType = "run_failed"
)

// Event is the payload pushed into a bot's event channel. The bot renders
// the event into a platform-native card and delivers it.
type Event struct {
	Type      EventType         `json:"type"`
	RunID     string            `json:"run_id"`
	Title     string            `json:"title"`
	Summary   string            `json:"summary"`
	Level     string            `json:"level,omitempty"`
	Status    string            `json:"status,omitempty"`
	Approver  string            `json:"approver,omitempty"`
	ChangeID  string            `json:"change_id,omitempty"`
	DetailURL string            `json:"detail_url,omitempty"`
	GateName  string            `json:"gate_name,omitempty"`
	GatePass  bool              `json:"gate_pass,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
}

// --- IncomingMessage --------------------------------------------------------

// IncomingMessage is a user-originated message received from a platform. The
// bot adapter is responsible for parsing the platform-native callback into
// this canonical shape before handing it to the command router.
type IncomingMessage struct {
	Platform Platform `json:"platform"`
	Channel  string   `json:"channel"` // group / channel id
	User     string   `json:"user"`    // user id (platform-native)
	UserName string   `json:"user_name"`
	Text     string   `json:"text"` // raw message text
	ThreadTS string   `json:"thread_ts,omitempty"`
	Raw      any      `json:"raw,omitempty"` // original payload, for adapters
}

// --- CommandResult ----------------------------------------------------------

// CommandResult is the outcome of dispatching an IncomingMessage through the
// command router. It carries a human-readable reply and an optional card
// payload that the bot may render.
type CommandResult struct {
	Reply string `json:"reply"`
	Card  *Card  `json:"card,omitempty"`
}

// --- CommandHandler ---------------------------------------------------------

// CommandHandler executes a parsed command and returns a result that the bot
// sends back to the user. Implementations typically call into the approval
// or state services.
type CommandHandler func(ctx context.Context, msg IncomingMessage, args []string) (CommandResult, error)

// --- Bot interface ----------------------------------------------------------

// Bot is the abstraction implemented by every platform adapter. A Bot
// receives LEVEE events through a channel (SubscribeEvents), renders them to
// platform-native cards and delivers them; it also exposes SendMessage for
// ad-hoc text delivery and HandleMessage for routing user-originated
// messages through the command dispatcher.
type Bot interface {
	// Name returns the bot's unique name (the registration key).
	Name() string
	// Platform returns the platform identifier.
	Platform() Platform
	// Start begins consuming events and (for webhook-style adapters) listening
	// for callbacks. It must be safe to call once; calling it twice returns an
	// error.
	Start(ctx context.Context) error
	// Stop shuts the bot down gracefully, draining in-flight events.
	Stop() error
	// SubscribeEvents returns the channel the BotManager pushes events into.
	// Adapters consume this channel in their Start loop.
	SubscribeEvents() <-chan Event
	// PublishEvent delivers an event to the bot's internal channel. It is
	// primarily used by BotManager.BroadcastEvent.
	PublishEvent(evt Event) error
	// SendMessage delivers an ad-hoc text message to a channel.
	SendMessage(ctx context.Context, channel, text string) error
	// SendCard delivers a rendered card to a channel.
	SendCard(ctx context.Context, channel string, card *Card) error
	// HandleMessage routes a user-originated message through the command
	// dispatcher and sends the reply back.
	HandleMessage(ctx context.Context, msg IncomingMessage) error
}

// --- BotManager -------------------------------------------------------------

// BotManager owns the set of registered Bot adapters and the command router.
// It is safe for concurrent use.
type BotManager struct {
	mu     sync.RWMutex
	bots   map[string]Bot
	router *CommandRouter
}

// NewBotManager returns an empty BotManager with a fresh command router.
func NewBotManager() *BotManager {
	return &BotManager{
		bots:   make(map[string]Bot),
		router: NewCommandRouter(),
	}
}

// Router returns the command router so callers can register custom handlers.
func (m *BotManager) Router() *CommandRouter {
	return m.router
}

// Register adds a bot to the manager. It returns ErrBotExists if a bot with
// the same name is already registered.
func (m *BotManager) Register(b Bot) error {
	if b == nil {
		return fmt.Errorf("chatops: register: %w", ErrBotNotFound)
	}
	name := b.Name()
	if name == "" {
		return fmt.Errorf("chatops: register: %w", ErrBotNotFound)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.bots[name]; exists {
		return fmt.Errorf("chatops: register %q: %w", name, ErrBotExists)
	}
	m.bots[name] = b
	log.Info("chatops: registered bot", "name", name, "platform", b.Platform())
	return nil
}

// Unregister removes a bot by name.
func (m *BotManager) Unregister(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.bots[name]; !exists {
		return fmt.Errorf("chatops: unregister %q: %w", name, ErrBotNotFound)
	}
	delete(m.bots, name)
	log.Info("chatops: unregistered bot", "name", name)
	return nil
}

// Get returns the bot with the given name.
func (m *BotManager) Get(name string) (Bot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.bots[name]
	if !ok {
		return nil, fmt.Errorf("chatops: get %q: %w", name, ErrBotNotFound)
	}
	return b, nil
}

// ByPlatform returns the first registered bot for the given platform. If
// multiple bots exist for the same platform the one with the lexically
// smallest name is returned.
func (m *BotManager) ByPlatform(p Platform) (Bot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var pick Bot
	for _, b := range m.bots {
		if b.Platform() != p {
			continue
		}
		if pick == nil || b.Name() < pick.Name() {
			pick = b
		}
	}
	if pick == nil {
		return nil, fmt.Errorf("chatops: by platform %q: %w", p, ErrBotNotFound)
	}
	return pick, nil
}

// Names returns the names of all registered bots in lexical order.
func (m *BotManager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.bots))
	for name := range m.bots {
		names = append(names, name)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return names
}

// StartAll starts every registered bot. If any bot fails to start the
// already-started bots are stopped and the error is returned.
func (m *BotManager) StartAll(ctx context.Context) error {
	m.mu.RLock()
	snapshot := make([]Bot, 0, len(m.bots))
	for _, b := range m.bots {
		snapshot = append(snapshot, b)
	}
	m.mu.RUnlock()

	started := make([]Bot, 0, len(snapshot))
	for _, b := range snapshot {
		if err := b.Start(ctx); err != nil {
			// Roll back.
			for _, s := range started {
				_ = s.Stop()
			}
			return fmt.Errorf("chatops: start %q: %w", b.Name(), err)
		}
		started = append(started, b)
	}
	return nil
}

// StopAll stops every registered bot, aggregating errors.
func (m *BotManager) StopAll() error {
	m.mu.RLock()
	snapshot := make([]Bot, 0, len(m.bots))
	for _, b := range m.bots {
		snapshot = append(snapshot, b)
	}
	m.mu.RUnlock()

	var errs []error
	for _, b := range snapshot {
		if err := b.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("chatops: stop %q: %w", b.Name(), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("chatops: stop all: %w (first: %v)", ErrBotClosed, errs[0])
	}
	return nil
}

// BroadcastEvent pushes an event to every registered bot. Bots whose event
// channel is full are skipped with a warning so a slow bot cannot block the
// manager.
func (m *BotManager) BroadcastEvent(evt Event) error {
	m.mu.RLock()
	snapshot := make([]Bot, 0, len(m.bots))
	for _, b := range m.bots {
		snapshot = append(snapshot, b)
	}
	m.mu.RUnlock()

	var errs []error
	for _, b := range snapshot {
		if err := b.PublishEvent(evt); err != nil {
			errs = append(errs, fmt.Errorf("chatops: broadcast %q: %w", b.Name(), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("chatops: broadcast: %w (first: %v)", ErrBotClosed, errs[0])
	}
	return nil
}

// HandleMessage routes a user-originated message to the bot identified by
// msg.Platform (the first matching bot). The reply is sent back through the
// same bot.
func (m *BotManager) HandleMessage(ctx context.Context, msg IncomingMessage) error {
	b, err := m.ByPlatform(msg.Platform)
	if err != nil {
		return err
	}
	return b.HandleMessage(ctx, msg)
}

// --- CommandRouter ----------------------------------------------------------

// CommandRouter parses user messages and dispatches them to registered
// handlers. The built-in command prefix is "/levee". Messages that do not
// start with the prefix are ignored (return nil with an empty reply).
type CommandRouter struct {
	mu       sync.RWMutex
	handlers map[string]CommandHandler
	prefix   string
}

// NewCommandRouter returns a router with the default "/levee" prefix and the
// built-in commands registered as no-op handlers. Callers replace the no-op
// handlers with real ones (backed by the approval / state services) via
// Register.
func NewCommandRouter() *CommandRouter {
	r := &CommandRouter{
		handlers: make(map[string]CommandHandler),
		prefix:   "/levee",
	}
	r.handlers["list"] = builtinList
	r.handlers["approve"] = builtinApprove
	r.handlers["reject"] = builtinReject
	r.handlers["status"] = builtinStatus
	r.handlers["help"] = builtinHelp
	return r
}

// Prefix returns the command prefix.
func (r *CommandRouter) Prefix() string { return r.prefix }

// SetPrefix overrides the command prefix. Must start with "/".
func (r *CommandRouter) SetPrefix(p string) error {
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("chatops: prefix must start with /: %q", p)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prefix = p
	return nil
}

// Register adds (or replaces) a handler for the given verb.
func (r *CommandRouter) Register(verb string, h CommandHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[verb] = h
}

// Verbs returns the registered verbs in lexical order.
func (r *CommandRouter) Verbs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	verbs := make([]string, 0, len(r.handlers))
	for v := range r.handlers {
		verbs = append(verbs, v)
	}
	for i := 1; i < len(verbs); i++ {
		for j := i; j > 0 && verbs[j-1] > verbs[j]; j-- {
			verbs[j-1], verbs[j] = verbs[j], verbs[j-1]
		}
	}
	return verbs
}

// Parse splits a raw message into (verb, args). It returns ErrEmptyCommand
// when the message is blank or does not start with the prefix, and
// ErrUnknownCommand when the verb is not registered.
func (r *CommandRouter) Parse(text string) (verb string, args []string, err error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil, fmt.Errorf("chatops: parse: %w", ErrEmptyCommand)
	}
	r.mu.RLock()
	prefix := r.prefix
	r.mu.RUnlock()

	if !strings.HasPrefix(text, prefix) {
		return "", nil, fmt.Errorf("chatops: parse: %w", ErrEmptyCommand)
	}
	rest := strings.TrimSpace(strings.TrimPrefix(text, prefix))
	if rest == "" {
		return "", nil, fmt.Errorf("chatops: parse: %w", ErrEmptyCommand)
	}
	parts := strings.Fields(rest)
	verb = parts[0]
	args = parts[1:]

	r.mu.RLock()
	_, ok := r.handlers[verb]
	r.mu.RUnlock()
	if !ok {
		return verb, args, fmt.Errorf("chatops: parse %q: %w", verb, ErrUnknownCommand)
	}
	return verb, args, nil
}

// Dispatch parses and executes a command. The reply is returned to the
// caller (the bot adapter) which is responsible for sending it back to the
// user.
func (r *CommandRouter) Dispatch(ctx context.Context, msg IncomingMessage) (CommandResult, error) {
	verb, args, err := r.Parse(msg.Text)
	if err != nil {
		// Non-command messages yield an empty result, not an error, so that
		// bots can ignore them quietly. Only return errors for unknown
		// commands so the bot can surface a "did you mean?" hint.
		if errors.Is(err, ErrUnknownCommand) {
			return CommandResult{Reply: fmt.Sprintf("unknown command %q. try %s help", verb, r.prefix)}, err
		}
		return CommandResult{}, nil
	}

	r.mu.RLock()
	h, ok := r.handlers[verb]
	r.mu.RUnlock()
	if !ok {
		return CommandResult{Reply: fmt.Sprintf("unknown command %q", verb)},
			fmt.Errorf("chatops: dispatch %q: %w", verb, ErrUnknownCommand)
	}

	result, err := h(ctx, msg, args)
	if err != nil {
		return CommandResult{Reply: fmt.Sprintf("command %q failed: %v", verb, err)},
			fmt.Errorf("chatops: dispatch %q: %w", verb, err)
	}
	return result, nil
}

// --- Built-in command handlers ----------------------------------------------

// builtinList is the default /levee list handler. It returns a placeholder
// reply; callers override it with a handler backed by the state store.
func builtinList(ctx context.Context, msg IncomingMessage, args []string) (CommandResult, error) {
	return CommandResult{Reply: "list: no handler registered (override via Router().Register(\"list\", ...))"}, nil
}

// builtinApprove is the default /levee approve <id> handler.
func builtinApprove(ctx context.Context, msg IncomingMessage, args []string) (CommandResult, error) {
	if len(args) < 1 {
		return CommandResult{Reply: "usage: /levee approve <change-id>"}, nil
	}
	return CommandResult{Reply: fmt.Sprintf("approve: no handler registered (change-id=%s)", args[0])}, nil
}

// builtinReject is the default /levee reject <id> [--reason r] handler.
func builtinReject(ctx context.Context, msg IncomingMessage, args []string) (CommandResult, error) {
	if len(args) < 1 {
		return CommandResult{Reply: "usage: /levee reject <change-id> [--reason <text>]"}, nil
	}
	return CommandResult{Reply: fmt.Sprintf("reject: no handler registered (change-id=%s)", args[0])}, nil
}

// builtinStatus is the default /levee status <id> handler.
func builtinStatus(ctx context.Context, msg IncomingMessage, args []string) (CommandResult, error) {
	if len(args) < 1 {
		return CommandResult{Reply: "usage: /levee status <change-id>"}, nil
	}
	return CommandResult{Reply: fmt.Sprintf("status: no handler registered (change-id=%s)", args[0])}, nil
}

// builtinHelp lists the available verbs.
func builtinHelp(ctx context.Context, msg IncomingMessage, args []string) (CommandResult, error) {
	return CommandResult{Reply: "available commands: /levee list | approve <id> | reject <id> | status <id> | help"}, nil
}

// --- baseBot ---------------------------------------------------------------

// baseBot embeds the shared machinery every platform adapter needs: an event
// channel, a started flag, a router reference and a stop channel. Adapters
// compose baseBot and implement the platform-specific Send* methods.
type baseBot struct {
	name     string
	platform Platform
	router   *CommandRouter

	mu        sync.Mutex
	started   bool
	eventCh   chan Event
	stopCh    chan struct{}
	closeOnce sync.Once
}

// newBaseBot returns a baseBot with an unbuffered event channel. Adapters
// may swap in a buffered channel by setting eventCh after construction.
func newBaseBot(name string, platform Platform, router *CommandRouter, buf int) baseBot {
	if router == nil {
		router = NewCommandRouter()
	}
	return baseBot{
		name:     name,
		platform: platform,
		router:   router,
		eventCh:  make(chan Event, buf),
		stopCh:   make(chan struct{}),
	}
}

// Name satisfies part of the Bot interface.
func (b *baseBot) Name() string { return b.name }

// Platform satisfies part of the Bot interface.
func (b *baseBot) Platform() Platform { return b.platform }

// SubscribeEvents returns the event channel.
func (b *baseBot) SubscribeEvents() <-chan Event { return b.eventCh }

// PublishEvent pushes an event into the channel. It returns ErrBotClosed
// when the bot has been stopped.
func (b *baseBot) PublishEvent(evt Event) error {
	b.mu.Lock()
	closed := b.isClosedLocked()
	b.mu.Unlock()
	if closed {
		return fmt.Errorf("chatops: publish %q: %w", b.name, ErrBotClosed)
	}
	select {
	case b.eventCh <- evt:
		return nil
	default:
		// Channel full: drop and warn rather than block the manager.
		log.Warn("chatops: event channel full, dropping event",
			"bot", b.name, "event", evt.Type, "run_id", evt.RunID)
		return nil
	}
}

// markStarted flips the started flag. It returns an error if already started.
func (b *baseBot) markStarted() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started {
		return fmt.Errorf("chatops: bot %q already started", b.name)
	}
	b.started = true
	return nil
}

// markStopped flips the started flag and closes stopCh exactly once.
func (b *baseBot) markStopped() {
	b.closeOnce.Do(func() {
		close(b.stopCh)
	})
	b.mu.Lock()
	b.started = false
	b.mu.Unlock()
}

// isClosedLocked reports whether stopCh has been closed. Caller must hold b.mu.
func (b *baseBot) isClosedLocked() bool {
	select {
	case <-b.stopCh:
		return true
	default:
		return false
	}
}

// stopChan returns the stop channel so adapters can select on it in their
// event loop.
func (b *baseBot) stopChan() <-chan struct{} { return b.stopCh }

// drainEvents drains any remaining events after stop. Adapters call it from
// their Stop method.
func (b *baseBot) drainEvents() {
	for {
		select {
		case <-b.eventCh:
		default:
			return
		}
	}
}
