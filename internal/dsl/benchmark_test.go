package dsl

import (
	"fmt"
	"testing"
)

// --- Parser benchmarks ---

const benchWorkflowYAML = `name: deploy-nginx
version: "1.0"
description: Deploy Nginx to web tier
input:
  - name: version
    type: string
    required: true
target:
  name: web
  type: ssh
  hosts:
    - web1.example.com
    - web2.example.com
    - web3.example.com
window:
  start: "2024-01-01T02:00:00Z"
  end: "2024-01-01T06:00:00Z"
  timezone: UTC
  max_concurrency: 5
batches:
  strategy: percent
  steps: [1, 10, 50, 100]
approval:
  level: standard
  approvers: [alice, bob]
  min_approvers: 1
steps:
  - name: install-nginx
    action: pkg.install
    args:
      name: nginx
      version: "{{ .Input.version }}"
    idempotent: true
  - name: deploy-config
    action: file.copy
    args:
      src: /local/nginx.conf
      dest: /etc/nginx/nginx.conf
    idempotent: true
  - name: restart-nginx
    action: svc.restart
    args:
      name: nginx
`

func BenchmarkParser_ParseBytes(b *testing.B) {
	b.ReportAllocs()
	p := NewParser()
	data := []byte(benchWorkflowYAML)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = p.ParseBytes(data)
	}
}

func BenchmarkParser_ParseBytes_Large(b *testing.B) {
	b.ReportAllocs()
	p := NewParser()
	// Generate a larger workflow with 50 steps
	largeYAML := generateLargeWorkflowYAML(50)
	data := []byte(largeYAML)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = p.ParseBytes(data)
	}
}

func BenchmarkNewParser(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = NewParser()
	}
}

// --- Validator benchmarks ---

func BenchmarkValidator_Validate(b *testing.B) {
	b.ReportAllocs()
	p := NewParser()
	wf, _ := p.ParseBytes([]byte(benchWorkflowYAML))
	v := NewValidator()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v.Validate(wf)
	}
}

func BenchmarkValidator_ValidateStrict(b *testing.B) {
	b.ReportAllocs()
	p := NewParser()
	wf, _ := p.ParseBytes([]byte(benchWorkflowYAML))
	v := NewValidator()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v.ValidateStrict(wf)
	}
}

func BenchmarkNewValidator(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = NewValidator()
	}
}

// --- AST conversion benchmarks ---

func BenchmarkSplitAction(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = splitAction("pkg.install")
	}
}

func BenchmarkConvertInput_List(b *testing.B) {
	b.ReportAllocs()
	raw := []any{
		map[string]any{"name": "v1", "type": "string", "required": true},
		map[string]any{"name": "v2", "type": "int"},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = convertInput(raw)
	}
}

// --- helpers ---

func generateLargeWorkflowYAML(stepCount int) string {
	yaml := `name: large-workflow
version: "1.0"
target:
  name: web
  type: ssh
  hosts:
    - host1
    - host2
    - host3
steps:
`
	for i := 0; i < stepCount; i++ {
		yaml += fmt.Sprintf("  - name: step-%d\n    action: shell.exec\n    args:\n      cmd: \"echo step-%d\"\n", i, i)
	}
	return yaml
}
