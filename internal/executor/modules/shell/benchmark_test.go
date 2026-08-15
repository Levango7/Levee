package shell

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/executor"
)

// --- Shell Module benchmarks ---

func BenchmarkShell_New(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = New()
	}
}

func BenchmarkShell_Name(b *testing.B) {
	b.ReportAllocs()
	m := New()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Name()
	}
}

func BenchmarkShell_Actions(b *testing.B) {
	b.ReportAllocs()
	m := New()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Actions()
	}
}

func BenchmarkShell_Idempotent(b *testing.B) {
	b.ReportAllocs()
	m := New()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Idempotent()
	}
}

func BenchmarkShell_Execute_Exec(b *testing.B) {
	b.ReportAllocs()
	m := New()
	ch := &benchChannel{}
	input := executor.ModuleInput{
		Action:  "exec",
		Args:    map[string]any{"cmd": "echo hello"},
		Channel: ch,
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = m.Execute(ctx, "exec", input)
	}
}

func BenchmarkStringArg(b *testing.B) {
	b.ReportAllocs()
	args := map[string]any{"cmd": "echo hello", "path": "/tmp/file"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = stringArg(args, "cmd")
	}
}

func BenchmarkRandomSuffix(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = randomSuffix()
	}
}

// --- test helpers ---

type benchChannel struct{}

func (c *benchChannel) Connect(ctx context.Context) error { return nil }
func (c *benchChannel) Exec(ctx context.Context, cmd string) (*channel.ExecResult, error) {
	return &channel.ExecResult{ExitCode: 0, Stdout: "ok"}, nil
}
func (c *benchChannel) Upload(ctx context.Context, remotePath string, content io.Reader) error {
	return nil
}
func (c *benchChannel) Download(ctx context.Context, remotePath string) (io.Reader, error) {
	return strings.NewReader(""), nil
}
func (c *benchChannel) Close() error      { return nil }
func (c *benchChannel) IsConnected() bool { return true }
