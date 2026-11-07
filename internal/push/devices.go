package push

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// DeviceStore persists the device registry so a registration made by one
// process is visible to the next.
//
// Why this exists: the registry used to live only in the PushManager's
// in-memory map. `levee push register` writes into that map and the process
// exits, so the registration was gone before the next command could use it —
// `levee push devices` always answered "no devices registered" and
// `levee push send` always failed with "device not found". The registry is
// deployment state (which phones belong to which approver), so it has to
// outlive the process that records it.
type DeviceStore interface {
	// Load returns the persisted registry, keyed by user ID. A registry that
	// does not exist yet is an empty registry, not an error.
	Load() (map[string][]DeviceToken, error)
	// Save replaces the persisted registry with the given contents.
	Save(devices map[string][]DeviceToken) error
}

// FileDeviceStore keeps the registry in a JSON file.
//
// The file is written atomically (temp file in the same directory, then
// rename) so a crash mid-write cannot leave a truncated registry behind: a
// half-written file would otherwise be read back as "corrupt" and take every
// registered device with it. Mode is 0600 because a device token is a
// credential for that device's push channel.
type FileDeviceStore struct {
	path string

	// mu serialises Load and Save against each other so a concurrent
	// read never observes a partially written file. The PushManager holds
	// its own lock for the in-memory map; this one only guards the file.
	mu sync.Mutex
}

// NewFileDeviceStore returns a store backed by the file at path. The file
// and its parent directory are created on the first Save; Load of a missing
// file is not an error.
func NewFileDeviceStore(path string) *FileDeviceStore {
	return &FileDeviceStore{path: path}
}

// Path returns the file the registry is stored in.
func (s *FileDeviceStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Load reads the registry. A missing file yields an empty registry; a
// corrupt file is an error — silently treating garbage as "no devices" would
// look exactly like the bug this store exists to fix.
func (s *FileDeviceStore) Load() (map[string][]DeviceToken, error) {
	if s == nil {
		return map[string][]DeviceToken{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]DeviceToken{}, nil
		}
		return nil, fmt.Errorf("push: read device registry %s: %w", s.path, err)
	}
	if len(data) == 0 {
		return map[string][]DeviceToken{}, nil
	}
	var devices map[string][]DeviceToken
	if err := json.Unmarshal(data, &devices); err != nil {
		return nil, fmt.Errorf("push: parse device registry %s: %w", s.path, err)
	}
	if devices == nil {
		devices = map[string][]DeviceToken{}
	}
	// A user with an empty device list is not a registration; drop it so the
	// file and the in-memory map have one shape.
	for userID, list := range devices {
		if len(list) == 0 {
			delete(devices, userID)
		}
	}
	return devices, nil
}

// Save writes the registry, replacing whatever was there. The write is
// atomic: readers see either the previous file or the new one.
func (s *FileDeviceStore) Save(devices map[string][]DeviceToken) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	payload := make(map[string][]DeviceToken, len(devices))
	for userID, list := range devices {
		if len(list) == 0 {
			continue
		}
		cp := make([]DeviceToken, len(list))
		copy(cp, list)
		payload[userID] = cp
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("push: encode device registry: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("push: create device registry dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".push_devices-*.tmp")
	if err != nil {
		return fmt.Errorf("push: create temp device registry: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: on any failure below the temp file must not be
	// left behind, or the data dir accumulates one orphan per failed write.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("push: write device registry: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("push: chmod device registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("push: close device registry: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("push: replace device registry %s: %w", s.path, err)
	}
	return nil
}
