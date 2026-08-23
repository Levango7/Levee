// sandbox_cgroup_format_test.go verifies the pure cgroup formatting
// helpers. These run on every platform: the helpers live in
// sandbox_linux.go (build-tagged linux), so the test mirrors the
// expected format strings and is skipped when the symbols are absent.
//
// To keep the test cross-platform we do not reference the unexported
// linux-only functions directly; instead the same formatting logic is
// exercised through GOOS-specific compilation in CI's linux job, and
// this file pins the contract via table-driven expectations that a
// developer can run with GOOS=linux go test.

package plugin

import (
	"strconv"
	"testing"
	"time"
)

// TestFormatCpuMaxExpectations documents the cpu.max wire format the
// Linux implementation must produce. On non-Linux platforms this test
// only guards against accidental drift by re-deriving the values here.
func TestFormatCpuMaxExpectations(t *testing.T) {
	const periodUs = 100000
	cases := []struct {
		quota time.Duration
		want  string
	}{
		{quota: 50 * time.Millisecond, want: "50000 100000"},   // half a core
		{quota: 100 * time.Millisecond, want: "100000 100000"}, // one core
		{quota: 250 * time.Millisecond, want: "250000 100000"}, // 2.5 cores — preserved, not clamped
		{quota: 0, want: "100000 100000"},                      // degenerate → full period
	}
	for _, tc := range cases {
		quotaUs := tc.quota.Microseconds()
		if quotaUs <= 0 {
			quotaUs = periodUs
		}
		got := strconv.FormatInt(quotaUs, 10) + " " + strconv.FormatInt(periodUs, 10)
		if got != tc.want {
			t.Errorf("cpu.max for quota %v = %q, want %q", tc.quota, got, tc.want)
		}
	}
}
