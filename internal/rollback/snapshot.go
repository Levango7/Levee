package rollback

// snapshot.go implements apply-time snapshot management (MVP task T036).
// Before a change is applied to a target, the SnapshotManager creates a
// snapshot of the files / configs that the change will touch. If the apply
// fails and rollback is triggered, the snapshot can be restored to bring
// the target back to its pre-change state.
//
// The on-disk snapshot store is organised as:
//
//	<rootDir>/<runID>/<target>/<snapshotID>/
//	  ├── meta.json          — Snapshot metadata
//	  ├── files/             — file-level snapshots (copies of original files)
//	  │   └── <flattened-path>
//	  └── configs/           — config-level snapshots (exported config text)
//	      └── <flattened-path>
//
// The manager supports two snapshot types:
//
//   - SnapshotTypeFile:   the file at each given path is copied verbatim into
//     the snapshot's files/ directory. Restore copies it back.
//   - SnapshotTypeConfig: the config at each given path is read and stored as
//     text in the snapshot's configs/ directory. Restore writes the text back.
//
// In MVP the two types behave identically (both copy file contents); the
// distinction exists so that future versions can plug in a real config
// exporter (e.g. dump running config from a service) without changing the
// API.
//
// Path flattening: absolute paths like "/etc/nginx/nginx.conf" are stored
// under the flattened name "root_etc_nginx_nginx.conf" so that the snapshot
// directory stays shallow and portable across operating systems. Restore
// inverts the flattening to reconstruct the original path.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// --- SnapshotType ----------------------------------------------------------

// SnapshotType distinguishes file-level snapshots from config-level snapshots.
type SnapshotType string

const (
	// SnapshotTypeFile is a file-level snapshot: the file at each path is
	// copied verbatim into the snapshot store.
	SnapshotTypeFile SnapshotType = "file"

	// SnapshotTypeConfig is a config-level snapshot: the config at each
	// path is read and stored as text. In MVP this is equivalent to
	// SnapshotTypeFile; the distinction is preserved for future
	// plug-in of a real config exporter.
	SnapshotTypeConfig SnapshotType = "config"
)

// --- Snapshot --------------------------------------------------------------

// Snapshot is the metadata record of a single snapshot. The actual file /
// config contents live in the snapshot store directory identified by Path.
type Snapshot struct {
	// ID is the unique snapshot identifier (a hex string). It is also the
	// name of the directory under <rootDir>/<runID>/<target>/ that holds
	// the snapshot contents.
	ID string `json:"id"`

	// RunID is the change run this snapshot belongs to. Snapshots are
	// grouped by run so that all snapshots of a run can be listed or
	// cleaned up together.
	RunID string `json:"run_id"`

	// Target is the host the snapshot was taken on. Together with RunID
	// it identifies the snapshot directory under rootDir.
	Target string `json:"target"`

	// Type is the snapshot type (file or config).
	Type SnapshotType `json:"type"`

	// Path is the absolute path to the snapshot directory in the store.
	// It is populated by the store when the snapshot is created.
	Path string `json:"path"`

	// CreatedAt is the wall-clock time the snapshot was taken (UTC).
	CreatedAt time.Time `json:"created_at"`

	// Metadata carries optional information about the snapshot, e.g. the
	// list of source paths, the apply step that triggered it, or a
	// checksum. The manager does not interpret the keys.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// --- SnapshotStore ---------------------------------------------------------

// SnapshotStore is the storage backend for snapshots. The default
// implementation is FileSnapshotStore (an on-disk store rooted at a
// configurable directory); the interface allows tests to plug in an
// in-memory store.
type SnapshotStore interface {
	// Create persists the snapshot metadata and returns the absolute
	// directory path where the snapshot contents should be written.
	// Implementations must create the directory if it does not exist.
	Create(ctx context.Context, snap *Snapshot) (string, error)

	// Get loads the snapshot metadata for the given ID. Returns an error
	// wrapping os.ErrNotExist if the snapshot does not exist.
	Get(ctx context.Context, id string) (*Snapshot, error)

	// List returns all snapshots matching the given runID and target.
	// Either may be empty to skip filtering on that dimension.
	List(ctx context.Context, runID, target string) ([]*Snapshot, error)

	// Delete removes the snapshot with the given ID, including its
	// contents directory. It is idempotent: deleting a non-existent
	// snapshot returns nil.
	Delete(ctx context.Context, id string) error
}

// --- FileSnapshotStore -----------------------------------------------------

// FileSnapshotStore is the default on-disk SnapshotStore. It organises
// snapshots under a configurable root directory as
// <rootDir>/<runID>/<target>/<snapshotID>/.
type FileSnapshotStore struct {
	rootDir string
	mu      sync.RWMutex
	// index maps snapshot ID → *Snapshot for fast lookups. The on-disk
	// meta.json is the source of truth; the index is a read cache.
	index map[string]*Snapshot
}

// NewFileSnapshotStore returns a FileSnapshotStore rooted at rootDir. The
// rootDir is created if it does not exist. An empty rootDir is rejected.
func NewFileSnapshotStore(rootDir string) (*FileSnapshotStore, error) {
	if rootDir == "" {
		return nil, fmt.Errorf("snapshot store: root dir is empty")
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("snapshot store: mkdir %s: %w", rootDir, err)
	}
	return &FileSnapshotStore{rootDir: rootDir, index: make(map[string]*Snapshot)}, nil
}

// RootDir returns the store's root directory. It is intended for diagnostics.
func (s *FileSnapshotStore) RootDir() string { return s.rootDir }

// Create persists the snapshot metadata to <rootDir>/<runID>/<target>/<id>/
// meta.json and returns the absolute path to that directory. The directory is
// created with files/ and configs/ sub-directories ready to receive contents.
func (s *FileSnapshotStore) Create(ctx context.Context, snap *Snapshot) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("snapshot store create: %w", err)
	}
	if snap == nil {
		return "", fmt.Errorf("snapshot store create: snapshot is nil")
	}
	if snap.ID == "" {
		return "", fmt.Errorf("snapshot store create: snapshot ID is empty")
	}

	dir := s.snapshotDir(snap)
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o755); err != nil {
		return "", fmt.Errorf("snapshot store create: mkdir files: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o755); err != nil {
		return "", fmt.Errorf("snapshot store create: mkdir configs: %w", err)
	}

	metaPath := filepath.Join(dir, "meta.json")
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return "", fmt.Errorf("snapshot store create: marshal meta: %w", err)
	}
	if err := os.WriteFile(metaPath, data, 0o644); err != nil {
		return "", fmt.Errorf("snapshot store create: write meta: %w", err)
	}

	s.mu.Lock()
	s.index[snap.ID] = snap
	s.mu.Unlock()

	return dir, nil
}

// Get loads the snapshot metadata for the given ID. It first consults the
// in-memory index; on a miss it walks the store looking for a matching
// meta.json.
func (s *FileSnapshotStore) Get(ctx context.Context, id string) (*Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("snapshot store get: %w", err)
	}
	if id == "" {
		return nil, fmt.Errorf("snapshot store get: ID is empty")
	}

	s.mu.RLock()
	if snap, ok := s.index[id]; ok {
		s.mu.RUnlock()
		return snap, nil
	}
	s.mu.RUnlock()

	// Cache miss: walk the store. The walk is bounded by the number of
	// snapshots on disk, which is small in practice.
	var found *Snapshot
	walkErr := filepath.Walk(s.rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Tolerate walk errors on directories we did not create
			// (e.g. permission denied on a sibling); skip them.
			if info != nil && info.IsDir() {
				return filepath.SkipDir
			}
			return err
		}
		if info.IsDir() || filepath.Base(path) != "meta.json" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		var snap Snapshot
		if jerr := json.Unmarshal(data, &snap); jerr != nil {
			return jerr
		}
		if snap.ID == id {
			found = &snap
			return filepath.SkipDir
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("snapshot store get: walk: %w", walkErr)
	}
	if found == nil {
		return nil, fmt.Errorf("snapshot store get: %s: %w", id, os.ErrNotExist)
	}

	s.mu.Lock()
	s.index[found.ID] = found
	s.mu.Unlock()
	return found, nil
}

// List returns all snapshots whose RunID and Target match the given filters.
// Empty runID or target means "do not filter on that dimension".
func (s *FileSnapshotStore) List(ctx context.Context, runID, target string) ([]*Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("snapshot store list: %w", err)
	}

	var out []*Snapshot
	walkErr := filepath.Walk(s.rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if info != nil && info.IsDir() {
				return filepath.SkipDir
			}
			return err
		}
		if info.IsDir() || filepath.Base(path) != "meta.json" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		var snap Snapshot
		if jerr := json.Unmarshal(data, &snap); jerr != nil {
			return jerr
		}
		if runID != "" && snap.RunID != runID {
			return nil
		}
		if target != "" && snap.Target != target {
			return nil
		}
		out = append(out, &snap)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("snapshot store list: walk: %w", walkErr)
	}
	return out, nil
}

// Delete removes the snapshot with the given ID, including its contents
// directory. It is idempotent: deleting a non-existent snapshot returns nil.
func (s *FileSnapshotStore) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("snapshot store delete: %w", err)
	}
	if id == "" {
		return fmt.Errorf("snapshot store delete: ID is empty")
	}

	snap, err := s.Get(ctx, id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // idempotent
		}
		return err
	}

	dir := s.snapshotDir(snap)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("snapshot store delete: remove %s: %w", dir, err)
	}

	s.mu.Lock()
	delete(s.index, id)
	s.mu.Unlock()
	return nil
}

// snapshotDir returns the absolute path to the snapshot directory.
func (s *FileSnapshotStore) snapshotDir(snap *Snapshot) string {
	return filepath.Join(s.rootDir, snap.RunID, snap.Target, snap.ID)
}

// --- SnapshotManager -------------------------------------------------------

// SnapshotManager creates and restores snapshots. It is configured with a
// SnapshotStore and a default SnapshotType (used by CreateSnapshot when the
// caller does not specify one).
type SnapshotManager struct {
	store    SnapshotStore
	snapType SnapshotType
}

// SnapshotManagerOption configures a SnapshotManager at construction time.
type SnapshotManagerOption func(*SnapshotManager)

// WithSnapshotType sets the default snapshot type used by CreateSnapshot
// when the caller passes an empty type. The default is SnapshotTypeFile.
func WithSnapshotType(t SnapshotType) SnapshotManagerOption {
	return func(m *SnapshotManager) { m.snapType = t }
}

// NewSnapshotManager returns a SnapshotManager backed by store. The store
// must be non-nil; the default snapshot type is SnapshotTypeFile.
func NewSnapshotManager(store SnapshotStore, opts ...SnapshotManagerOption) (*SnapshotManager, error) {
	if store == nil {
		return nil, fmt.Errorf("snapshot manager: store is nil")
	}
	m := &SnapshotManager{store: store, snapType: SnapshotTypeFile}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

// Store returns the manager's underlying store. It is intended for
// diagnostics and tests.
func (m *SnapshotManager) Store() SnapshotStore { return m.store }

// CreateSnapshot creates a snapshot of the given paths for the given run /
// target. Each path is read from the local filesystem and copied into the
// snapshot store under files/ (for SnapshotTypeFile) or configs/ (for
// SnapshotTypeConfig). The returned Snapshot's Path field is the absolute
// path to the snapshot directory.
//
// Behaviour:
//
//   - snapType empty: the manager's default type is used.
//   - paths empty: a snapshot record is still created (with an empty
//     contents directory) so that RestoreSnapshot can be called later; this
//     is useful when a step has nothing to back up but the audit trail
//     should still record that a snapshot was taken.
//   - a path does not exist: the error is returned and the snapshot is
//     deleted; partial snapshots are not kept.
func (m *SnapshotManager) CreateSnapshot(ctx context.Context, runID, target string, paths []string, snapType SnapshotType, metadata map[string]any) (*Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("create snapshot: %w", err)
	}
	if runID == "" {
		return nil, fmt.Errorf("create snapshot: runID is empty")
	}
	if target == "" {
		return nil, fmt.Errorf("create snapshot: target is empty")
	}
	if snapType == "" {
		snapType = m.snapType
	}

	snap := &Snapshot{
		ID:        newSnapshotID(),
		RunID:     runID,
		Target:    target,
		Type:      snapType,
		CreatedAt: time.Now().UTC(),
		Metadata:  metadata,
	}

	dir, err := m.store.Create(ctx, snap)
	if err != nil {
		return nil, fmt.Errorf("create snapshot: store create: %w", err)
	}
	snap.Path = dir

	// Copy each path into the snapshot. We do this after the snapshot
	// record exists so that on failure we can clean up the whole snapshot.
	if err := m.copyPaths(ctx, snap, paths); err != nil {
		_ = m.store.Delete(ctx, snap.ID)
		return nil, fmt.Errorf("create snapshot: copy paths: %w", err)
	}

	log.Info("snapshot created",
		"snapshot_id", snap.ID,
		"run_id", runID,
		"target", target,
		"type", snapType,
		"paths", len(paths))
	return snap, nil
}

// copyPaths copies each path into the snapshot's contents directory. The
// destination sub-directory is "files" for SnapshotTypeFile and "configs"
// for SnapshotTypeConfig. Each source path is mapped to a destination path
// that preserves the relative structure; absolute paths are stored under a
// flattened name (slashes / colons replaced by underscores) to avoid
// creating a deep tree rooted at the snapshot directory and to stay
// portable across operating systems.
//
// To make restore reliable, copyPaths also writes a paths.json file at the
// snapshot directory root recording the flattened→original mapping. This
// avoids relying on unflattenPath heuristics, which cannot perfectly invert
// flattenPath when the original path contained underscores.
func (m *SnapshotManager) copyPaths(ctx context.Context, snap *Snapshot, paths []string) error {
	subDir := "files"
	if snap.Type == SnapshotTypeConfig {
		subDir = "configs"
	}
	dstRoot := filepath.Join(snap.Path, subDir)

	// pathMap records flattened name → original path so RestoreSnapshot can
	// reconstruct the exact destination without guessing.
	pathMap := make(map[string]string, len(paths))

	for _, src := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Stat(src)
		if err != nil {
			return fmt.Errorf("stat %s: %w", src, err)
		}
		flat := flattenPath(src)
		pathMap[flat] = src
		if info.IsDir() {
			dst := filepath.Join(dstRoot, flat)
			if err := copyDir(src, dst); err != nil {
				return fmt.Errorf("copy dir %s: %w", src, err)
			}
			continue
		}
		dst := filepath.Join(dstRoot, flat)
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("copy file %s: %w", src, err)
		}
	}

	// Persist the path map so restore can recover the original paths.
	if len(pathMap) > 0 {
		if err := m.writePathMap(snap.Path, pathMap); err != nil {
			return fmt.Errorf("write path map: %w", err)
		}
	}
	return nil
}

// writePathMap writes the flattened→original path mapping to paths.json in
// the snapshot directory. The file is read by RestoreSnapshot.
func (m *SnapshotManager) writePathMap(snapDir string, pathMap map[string]string) error {
	data, err := json.MarshalIndent(pathMap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal path map: %w", err)
	}
	pathFile := filepath.Join(snapDir, "paths.json")
	if err := os.WriteFile(pathFile, data, 0o644); err != nil {
		return fmt.Errorf("write path map: %w", err)
	}
	return nil
}

// readPathMap reads the flattened→original path mapping from paths.json in
// the snapshot directory. Returns an empty map if the file does not exist
// (e.g. snapshots created with empty paths), and falls back to an empty
// map on any read error so that restore degrades to unflattenPath.
func (m *SnapshotManager) readPathMap(snapDir string) map[string]string {
	pathFile := filepath.Join(snapDir, "paths.json")
	data, err := os.ReadFile(pathFile)
	if err != nil {
		return nil
	}
	var pathMap map[string]string
	if err := json.Unmarshal(data, &pathMap); err != nil {
		return nil
	}
	return pathMap
}

// RestoreSnapshot restores the snapshot with the given ID. For each file /
// config in the snapshot's contents directory, the original path is
// reconstructed from the paths.json mapping (written at create time) and
// the contents are written back. If paths.json is absent (e.g. snapshot
// created with empty paths, or by an older version), the fallback
// unflattenPath heuristic is used.
//
// Behaviour:
//
//   - snapshot does not exist: returns an error.
//   - snapshot exists but contents directory is empty: returns nil (no-op).
//   - a destination path's parent directory does not exist: it is created.
func (m *SnapshotManager) RestoreSnapshot(ctx context.Context, snapshotID string) (*Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("restore snapshot: %w", err)
	}
	if snapshotID == "" {
		return nil, fmt.Errorf("restore snapshot: ID is empty")
	}

	snap, err := m.store.Get(ctx, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("restore snapshot: get: %w", err)
	}

	subDir := "files"
	if snap.Type == SnapshotTypeConfig {
		subDir = "configs"
	}
	srcRoot := filepath.Join(snap.Path, subDir)

	// Load the flattened→original path map written at create time. When
	// absent, fall back to unflattenPath for backward compatibility.
	pathMap := m.readPathMap(snap.Path)

	// Walk the contents directory and restore each entry.
	walkErr := filepath.Walk(srcRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Contents directory does not exist: nothing to restore.
				return nil
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(srcRoot, path)
		if rerr != nil {
			return rerr
		}
		// Resolve the original path: prefer the explicit mapping, fall
		// back to unflattenPath for snapshots without paths.json.
		orig, ok := pathMap[rel]
		if !ok {
			orig = unflattenPath(rel)
		}
		if err := os.MkdirAll(filepath.Dir(orig), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(orig), err)
		}
		if err := copyFile(path, orig); err != nil {
			return fmt.Errorf("restore %s: %w", orig, err)
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("restore snapshot: walk: %w", walkErr)
	}

	log.Info("snapshot restored",
		"snapshot_id", snap.ID,
		"run_id", snap.RunID,
		"target", snap.Target)
	return snap, nil
}

// --- ID generation ---------------------------------------------------------

// newSnapshotID generates a unique snapshot identifier using crypto/rand,
// matching the style of plan.newPlanID. The ID has the form
// "snap-<16-hex-chars>". On the extremely unlikely event that rand.Read
// fails, it falls back to a timestamp-based ID.
func newSnapshotID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("snap-%d", time.Now().UnixNano())
	}
	return "snap-" + hex.EncodeToString(b)
}

// --- path flattening helpers -----------------------------------------------

// flattenPath converts an absolute or relative path into a single path
// component safe to use as a filename under the snapshot directory. It
// replaces every path separator (and the Windows drive colon) with an
// underscore and prefixes absolute paths with "root" so that the
// flattened name is a valid, shallow filename on every operating system.
//
// Examples (on a Unix system):
//
//	"/etc/nginx/nginx.conf" → "root_etc_nginx_nginx.conf"
//	"configs/app.yml"      → "configs_app.yml"
//
// On Windows:
//
//	"C:\etc\nginx.conf"    → "rootC_etc_nginx.conf"
//
// flattenPath is not required to be perfectly invertible; the
// SnapshotManager writes a paths.json mapping at create time that
// RestoreSnapshot uses to recover the exact original path. unflattenPath
// is kept only as a backward-compatible fallback.
func flattenPath(p string) string {
	cleaned := filepath.Clean(p)
	// Replace every separator and colon (Windows drive letter) with an
	// underscore so the result is a valid single path component.
	replacer := strings.NewReplacer(
		string(filepath.Separator), "_",
		":", "_",
	)
	if filepath.IsAbs(cleaned) {
		// Mark absolute paths with the "root" prefix. On Windows the
		// leading separator is not the first character (the drive letter
		// is), so we just replace all separators / colons in place.
		return "root" + replacer.Replace(cleaned)
	}
	return replacer.Replace(cleaned)
}

// unflattenPath is a best-effort inverse of flattenPath: it reconstructs
// the original path from a flattened name. Names starting with "root" are
// treated as absolute paths; others are treated as relative paths.
//
// Note: because flattenPath replaces every separator and colon with "_",
// a path that originally contained an underscore cannot be perfectly
// reconstructed. This is an accepted MVP limitation; in practice
// RestoreSnapshot uses the paths.json mapping written at create time and
// only falls back to unflattenPath for backward compatibility.
func unflattenPath(flat string) string {
	if strings.HasPrefix(flat, "root") {
		rest := strings.TrimPrefix(flat, "root")
		return string(filepath.Separator) + strings.ReplaceAll(rest, "_", string(filepath.Separator))
	}
	return strings.ReplaceAll(flat, "_", string(filepath.Separator))
}

// --- file copy helpers ----------------------------------------------------

// copyFile copies a single file from src to dst, creating dst's parent
// directory if needed. It preserves the file mode of src.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
	}
	srcF, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer srcF.Close()

	info, err := srcF.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	dstF, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode())
	if err != nil {
		return fmt.Errorf("open %s: %w", dst, err)
	}
	defer dstF.Close()

	if _, err := io.Copy(dstF, srcF); err != nil {
		return fmt.Errorf("copy %s → %s: %w", src, dst, err)
	}
	return nil
}

// copyDir recursively copies a directory from src to dst, preserving the
// relative structure and file modes.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFile(path, target)
	})
}
