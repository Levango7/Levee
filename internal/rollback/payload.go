// payload.go adds the remote-capture payload API to the snapshot store:
// WriteSnapshotPayloads / ReadSnapshotPayloads let a transport-aware
// caller (the wiring layer's channel-backed snapshotter) fill a snapshot
// directory with file contents pulled from a TARGET machine and read them
// back, using exactly the on-disk layout the local-FS copyPaths produces
// (files/<flattened> + paths.json). The manager's own RestoreSnapshot
// therefore works on remote-captured snapshots unchanged.

package rollback

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WriteSnapshotPayloads stores one snapshot file per entry of payloads
// (keyed by the ORIGINAL path on the target machine) under
// snapDir/files/, plus the paths.json mapping the restore path consumes.
// It is the remote-capture counterpart of the manager's copyPaths: same
// layout, different content source (bytes the caller fetched over a
// channel instead of the local filesystem).
//
// An empty payload map still writes paths.json (empty mapping) so that
// restore degrades to a documented no-op rather than a missing-map error.
func WriteSnapshotPayloads(snapDir string, payloads map[string]string) error {
	if snapDir == "" {
		return fmt.Errorf("snapshot payload: snapshot dir is empty")
	}
	filesRoot := filepath.Join(snapDir, "files")
	if err := os.MkdirAll(filesRoot, 0o750); err != nil {
		return fmt.Errorf("snapshot payload: mkdir %s: %w", filesRoot, err)
	}
	pathMap := make(map[string]string, len(payloads))
	for orig, content := range payloads {
		if orig == "" {
			continue
		}
		flat := flattenPath(orig)
		pathMap[flat] = orig
		dst := filepath.Join(filesRoot, flat)
		if err := os.WriteFile(dst, []byte(content), 0o600); err != nil {
			return fmt.Errorf("snapshot payload: write %s: %w", dst, err)
		}
	}
	return writePayloadPathMap(snapDir, pathMap)
}

// ReadSnapshotPayloads loads every captured file of the snapshot at
// snapDir back into a original-path -> content map. Missing files/
// directory yields an empty map (an audit-only snapshot with no declared
// paths), mirroring RestoreSnapshot's no-op semantics.
func ReadSnapshotPayloads(snapDir string) (map[string]string, error) {
	if snapDir == "" {
		return nil, fmt.Errorf("snapshot payload: snapshot dir is empty")
	}
	filesRoot := filepath.Join(snapDir, "files")
	entries, err := os.ReadDir(filesRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("snapshot payload: read %s: %w", filesRoot, err)
	}
	pathMap := readPayloadPathMap(snapDir)
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		flat := e.Name()
		orig := pathMap[flat]
		if orig == "" {
			// Backward-compatible fallback (same heuristic as the
			// manager's restore): unflattenPath is imperfect for
			// paths containing underscores, which is exactly why the
			// create path persists paths.json.
			orig = unflattenPath(flat)
		}
		content, err := os.ReadFile(filepath.Join(filesRoot, flat))
		if err != nil {
			return nil, fmt.Errorf("snapshot payload: read %s: %w", flat, err)
		}
		out[orig] = string(content)
	}
	return out, nil
}

// writePayloadPathMap persists the flattened -> original mapping at
// snapDir/paths.json (same format the manager writes at create time).
func writePayloadPathMap(snapDir string, pathMap map[string]string) error {
	data, err := json.MarshalIndent(pathMap, "", "  ")
	if err != nil {
		return fmt.Errorf("snapshot payload: marshal path map: %w", err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "paths.json"), data, 0o600); err != nil {
		return fmt.Errorf("snapshot payload: write path map: %w", err)
	}
	return nil
}

// readPayloadPathMap loads the paths.json mapping, degrading to an empty
// map when absent (same tolerance as the manager's readPathMap).
func readPayloadPathMap(snapDir string) map[string]string {
	data, err := os.ReadFile(filepath.Join(snapDir, "paths.json"))
	if err != nil {
		return nil
	}
	var pathMap map[string]string
	if err := json.Unmarshal(data, &pathMap); err != nil {
		return nil
	}
	return pathMap
}
