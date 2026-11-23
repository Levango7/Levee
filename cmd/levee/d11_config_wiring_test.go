package main

// D-11 wiring guard: every config field that is validated must either be
// consumed by production code or not exist. Two rules here:
//   - Audit.ExportFormat must be read outside internal/config (it drives the
//     audit export format default in cmd_audit.go).
//   - Credential.Storage and Credential.KeyDerivation were dead knobs (validated
//     but never consumed) and were REMOVED. They must not come back — a field
//     that validates without taking effect is the worst kind of config shape.
//   - Credential.Encryption is a real knob and must stay wired.

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/config"
)

func TestD11RemovedCredentialFields(t *testing.T) {
	ct := reflect.TypeOf(config.CredentialConfig{})
	_, storageExists := ct.FieldByName("Storage")
	_, kdfExists := ct.FieldByName("KeyDerivation")
	assert.False(t, storageExists, "credential.storage was a dead knob (validated, never used) and must stay removed")
	assert.False(t, kdfExists, "credential.key_derivation was a dead knob (validated, never used) and must stay removed")
	_, encExists := ct.FieldByName("Encryption")
	assert.True(t, encExists, "credential.encryption is the real, wired knob and must remain")
}

func TestD11AuditExportFormatIsWired(t *testing.T) {
	files := productionGoSources(t) // reused from channel_config_wiring_test.go
	var found bool
	for _, f := range files {
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		if strings.Contains(string(b), "cfg.Audit.ExportFormat") {
			found = true
			break
		}
	}
	assert.True(t, found, "audit.export_format is validated in config but must also be consumed by production code (cmd_audit.go resolveExportFormat)")
}

func TestD11EncryptionIsWired(t *testing.T) {
	files := productionGoSources(t)
	var found bool
	for _, f := range files {
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		if strings.Contains(string(b), "cfg.Credential.Encryption") {
			found = true
			break
		}
	}
	assert.True(t, found, "credential.encryption must be consumed by production code (cmd_serve.go)")
}
