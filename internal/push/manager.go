package push

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// Platform constants for device registration. They are lower-cased on input
// so callers may pass "iOS" / "Android" etc.
const (
	PlatformIOS     = "ios"
	PlatformAndroid = "android"
)

// DeviceToken is a single registered mobile device. A user may have multiple
// devices (e.g. a phone and a tablet); each is tracked independently.
type DeviceToken struct {
	Token        string    `json:"token"`
	Platform     string    `json:"platform"`
	UserID       string    `json:"user_id"`
	RegisteredAt time.Time `json:"registered_at"`
}

// PushMessage is the platform-agnostic message format used by callers. The
// PushManager resolves the recipient's devices and translates this into
// APNSNotification or FCMMessage as appropriate.
type PushMessage struct {
	UserID   string            `json:"user_id"`
	Title    string            `json:"title"`
	Body     string            `json:"body"`
	Data     map[string]string `json:"data,omitempty"`
	Category string            `json:"category,omitempty"`
}

// PushManager routes PushMessages to the right upstream client (APNs or FCM)
// based on each registered device's platform. It also owns the device
// registry. A PushManager is safe for concurrent use.
//
// The registry is held in memory and, when a DeviceStore is configured,
// mirrored to it on every change — so a registration made by one process is
// visible to the next. Without a store the registry is process-local, which
// is what the in-package tests use.
type PushManager struct {
	apns  *APNSClient
	fcm   *FCMClient
	store DeviceStore

	mu      sync.RWMutex
	devices map[string][]DeviceToken // key: user ID
}

// NewPushManager builds a PushManager with a process-local device registry.
// Either apns or fcm may be nil when the deployment only uses one platform;
// SendToUser will skip the missing channel and log a warning.
//
// Callers that need registrations to survive the process (the CLI and the
// daemon) should use NewPushManagerWithStore instead.
func NewPushManager(apns *APNSClient, fcm *FCMClient) *PushManager {
	pm, err := NewPushManagerWithStore(apns, fcm, nil)
	if err != nil {
		// Unreachable: a nil store never fails to load.
		panic("push: NewPushManagerWithStore with a nil store: " + err.Error())
	}
	return pm
}

// NewPushManagerWithStore builds a PushManager whose device registry is
// persisted through store. Devices already recorded in the store are loaded
// into the manager, so a fresh process starts with the deployment's existing
// registrations instead of an empty set.
//
// A nil store yields a process-local registry (the NewPushManager
// behaviour). A store that cannot be read is an error: starting up with an
// empty registry would make every registered device disappear for this
// process, which is the exact failure the store is meant to prevent.
func NewPushManagerWithStore(apns *APNSClient, fcm *FCMClient, store DeviceStore) (*PushManager, error) {
	pm := &PushManager{
		apns:    apns,
		fcm:     fcm,
		devices: make(map[string][]DeviceToken),
		store:   store,
	}
	if store == nil {
		return pm, nil
	}
	devices, err := store.Load()
	if err != nil {
		return nil, err
	}
	if devices != nil {
		pm.devices = devices
	}
	return pm, nil
}

// RegisterDevice associates a device token with a user. If the same (token,
// platform) pair already exists for the user, the call is a no-op. The
// platform is normalised to lower case.
func (pm *PushManager) RegisterDevice(userID, token, platform string) error {
	if userID == "" {
		return ErrEmptyUserID
	}
	if token == "" {
		return ErrEmptyDeviceToken
	}
	plat := normalisePlatform(platform)
	if plat != PlatformIOS && plat != PlatformAndroid {
		return fmt.Errorf("%w: %q (allowed: ios, android)", ErrUnknownPlatform, platform)
	}

	pm.mu.Lock()
	defer pm.mu.Unlock()

	existing := pm.devices[userID]
	for _, d := range existing {
		if d.Token == token && d.Platform == plat {
			return nil // already registered
		}
	}
	pm.devices[userID] = append(existing, DeviceToken{
		Token:        token,
		Platform:     plat,
		UserID:       userID,
		RegisteredAt: time.Now().UTC(),
	})
	if err := pm.persistLocked(); err != nil {
		// The registration did not stick, so the in-memory view must not
		// claim it either: a device that is "registered" in memory but
		// absent from the store would be sent to now and lost on the next
		// process start.
		pm.devices[userID] = existing
		if len(pm.devices[userID]) == 0 {
			delete(pm.devices, userID)
		}
		return err
	}
	log.Info("push: device registered",
		"user_id", userID, "platform", plat, "token_len", len(token))
	return nil
}

// persistLocked mirrors the in-memory registry into the configured store.
// The caller must hold pm.mu (write lock) so the snapshot it writes is the
// registry as of this change.
//
// A nil store means "process-local registry" and is not an error.
func (pm *PushManager) persistLocked() error {
	if pm.store == nil {
		return nil
	}
	snapshot := make(map[string][]DeviceToken, len(pm.devices))
	for userID, list := range pm.devices {
		if len(list) == 0 {
			continue
		}
		cp := make([]DeviceToken, len(list))
		copy(cp, list)
		snapshot[userID] = cp
	}
	return pm.store.Save(snapshot)
}

// UnregisterDevice removes a device token from a user's device list. Returns
// ErrDeviceNotFound when the token is not registered for the user.
func (pm *PushManager) UnregisterDevice(userID, token string) error {
	if userID == "" {
		return ErrEmptyUserID
	}
	if token == "" {
		return ErrEmptyDeviceToken
	}

	pm.mu.Lock()
	defer pm.mu.Unlock()

	existing := pm.devices[userID]
	for i, d := range existing {
		if d.Token == token {
			pm.devices[userID] = append(existing[:i], existing[i+1:]...)
			if len(pm.devices[userID]) == 0 {
				delete(pm.devices, userID)
			}
			if err := pm.persistLocked(); err != nil {
				// Put the device back: the store still has it, so
				// dropping it from memory would make the two views
				// disagree until the next process start.
				pm.devices[userID] = existing
				return err
			}
			log.Info("push: device unregistered",
				"user_id", userID, "token_len", len(token))
			return nil
		}
	}
	return fmt.Errorf("%w: user %q token %q", ErrDeviceNotFound, userID, token)
}

// Send delivers a PushMessage to every device registered for msg.UserID.
// Each device is dispatched on the appropriate channel; per-device errors are
// collected but do not abort the loop. Returns an aggregate error wrapping
// ErrPushFailed when at least one device failed, and ErrDeviceNotFound when
// the user has no registered devices at all — a message that reached nobody
// is a failure, not a success.
func (pm *PushManager) Send(ctx context.Context, msg PushMessage) error {
	if msg.UserID == "" {
		return ErrEmptyUserID
	}
	devices := pm.ListDevices(msg.UserID)
	if len(devices) == 0 {
		return fmt.Errorf("%w: user %q has no registered devices", ErrDeviceNotFound, msg.UserID)
	}

	var errs []string
	for _, d := range devices {
		if err := pm.sendOne(ctx, d, msg); err != nil {
			errs = append(errs, fmt.Sprintf("%s/%s: %v", d.Platform, d.Token, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %d/%d devices failed: %v",
			ErrPushFailed, len(errs), len(devices), errs)
	}
	return nil
}

// SendToUser is a convenience wrapper that builds a PushMessage from the
// supplied title/body/data and dispatches it. The Category field is left
// empty; callers wanting interactive buttons should use Send directly.
func (pm *PushManager) SendToUser(ctx context.Context, userID, title, body string, data map[string]string) error {
	return pm.Send(ctx, PushMessage{
		UserID: userID,
		Title:  title,
		Body:   body,
		Data:   data,
	})
}

// ListDevices returns a snapshot of the devices registered for the user. The
// returned slice is a copy and may be modified freely by the caller.
func (pm *PushManager) ListDevices(userID string) []DeviceToken {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	src := pm.devices[userID]
	if len(src) == 0 {
		return nil
	}
	out := make([]DeviceToken, len(src))
	copy(out, src)
	return out
}

// sendOne dispatches a message to a single device on its platform channel.
func (pm *PushManager) sendOne(ctx context.Context, d DeviceToken, msg PushMessage) error {
	switch d.Platform {
	case PlatformIOS:
		if pm.apns == nil {
			log.Warn("push: apns client not configured; skipping ios device",
				"user_id", msg.UserID, "token_len", len(d.Token))
			return nil
		}
		notif := APNSNotification{
			DeviceToken: d.Token,
			Alert:       APNSAlert{Title: msg.Title, Body: msg.Body},
			Sound:       "default",
			Category:    msg.Category,
			CustomData:  toStringInterfaceMap(msg.Data),
		}
		return pm.apns.Send(ctx, notif)
	case PlatformAndroid:
		if pm.fcm == nil {
			log.Warn("push: fcm client not configured; skipping android device",
				"user_id", msg.UserID, "token_len", len(d.Token))
			return nil
		}
		m := FCMMessage{
			Token:        d.Token,
			Notification: &FCMNotification{Title: msg.Title, Body: msg.Body},
			Data:         msg.Data,
		}
		if msg.Category != "" {
			m.Android = &AndroidConfig{Priority: "high", ClickAction: msg.Category}
		}
		return pm.fcm.Send(ctx, m)
	default:
		return fmt.Errorf("%w: %q", ErrUnknownPlatform, d.Platform)
	}
}

// normalisePlatform lower-cases the platform string so callers may pass "iOS"
// or "iOS" interchangeably.
func normalisePlatform(p string) string {
	out := make([]byte, len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

// toStringInterfaceMap converts map[string]string to map[string]interface{}
// for the APNs CustomData field. Returns nil when the input is empty.
func toStringInterfaceMap(m map[string]string) map[string]interface{} {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
