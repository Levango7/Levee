package executor

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/nexus/levee/internal/channel"
)

// --- Executor benchmarks ---

func BenchmarkNewExecutor(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = NewExecutor()
	}
}

func BenchmarkExecutor_RegisterModule(b *testing.B) {
	b.ReportAllocs()
	e := NewExecutor()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := &benchModule{name: "mod"}
		e.RegisterModule(m)
	}
}

func BenchmarkExecutor_Module(b *testing.B) {
	b.ReportAllocs()
	e := NewExecutor()
	for i := 0; i < 20; i++ {
		e.RegisterModule(&benchModule{name: fmt.Sprintf("mod-%02d", i)})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = e.Module(fmt.Sprintf("mod-%02d", i%20))
	}
}

func BenchmarkExecutor_Modules(b *testing.B) {
	b.ReportAllocs()
	e := NewExecutor()
	for i := 0; i < 20; i++ {
		e.RegisterModule(&benchModule{name: fmt.Sprintf("mod-%02d", i)})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = e.Modules()
	}
}

func BenchmarkExecutor_Execute(b *testing.B) {
	b.ReportAllocs()
	e := NewExecutor()
	e.RegisterModule(&benchModule{name: "noop"})
	input := ModuleInput{
		Action:  "exec",
		Args:    map[string]any{"cmd": "echo ok"},
		Channel: &benchChannel{},
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = e.Execute(ctx, "noop", "exec", input)
	}
}

func BenchmarkActionSupported(b *testing.B) {
	b.ReportAllocs()
	m := &benchModule{name: "noop"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = actionSupported(m, "exec")
	}
}

// --- ShellRunner benchmarks ---

func BenchmarkNewShellRunner(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = NewShellRunner()
	}
}

func BenchmarkBuildShellCommand(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = buildShellCommand("echo hello")
	}
}

func BenchmarkIsBlank(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = isBlank("echo hello")
	}
}

// --- test helpers ---

type benchModule struct {
	name string
}

func (m *benchModule) Name() string      { return m.name }
func (m *benchModule) Actions() []string { return []string{"exec", "script"} }
func (m *benchModule) Idempotent() bool  { return false }
func (m *benchModule) Execute(ctx context.Context, action string, input ModuleInput) (*ModuleOutput, error) {
	return &ModuleOutput{ExitCode: 0, Stdout: "ok", Changed: true}, nil
}

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
