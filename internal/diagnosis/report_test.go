package diagnosis

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDiagnosticReportEmptyCollectionsMarshalAsArrays pins the JSON contract
// of the report: every collection field without omitempty must encode as []
// when empty, never null. Consumers iterate these fields without a nil check,
// and "the probe ran and found nothing" must not look like "the field never
// arrived". A zero report is reachable: `diagnose --logs` skips the health
// probe entirely, `--health` skips log analysis, and individual probes
// degrade to empty sub-reports (e.g. no services configured, unreadable
// disks, a failed DB probe).
func TestDiagnosticReportEmptyCollectionsMarshalAsArrays(t *testing.T) {
	raw, err := json.Marshal(DiagnosticReport{})
	require.NoError(t, err)

	var decoded struct {
		Health struct {
			Network struct {
				TCP json.RawMessage `json:"tcp"`
			} `json:"network"`
			Node struct {
				Disks json.RawMessage `json:"disks"`
			} `json:"node"`
			Service struct {
				Services json.RawMessage `json:"services"`
			} `json:"service"`
			Data struct {
				Disks json.RawMessage `json:"disks"`
			} `json:"data"`
		} `json:"health"`
		Findings        json.RawMessage `json:"findings"`
		Recommendations json.RawMessage `json:"recommendations"`
	}
	require.NoError(t, json.Unmarshal(raw, &decoded), "document: %s", raw)

	for name, got := range map[string]json.RawMessage{
		"health.network.tcp":      decoded.Health.Network.TCP,
		"health.node.disks":       decoded.Health.Node.Disks,
		"health.service.services": decoded.Health.Service.Services,
		"health.data.disks":       decoded.Health.Data.Disks,
		"findings":                decoded.Findings,
		"recommendations":         decoded.Recommendations,
	} {
		assert.Equal(t, "[]", string(got), "%s must be [], not null; document: %s", name, raw)
	}
}
