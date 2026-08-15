package permission

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// --- PermissionMatrix benchmarks ---

func BenchmarkNewPermissionMatrix(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = NewPermissionMatrix()
	}
}

func BenchmarkPermissionMatrix_Grant(b *testing.B) {
	b.ReportAllocs()
	m := NewPermissionMatrix()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Grant("sre", "prod", "apply")
	}
}

func BenchmarkPermissionMatrix_Allow(b *testing.B) {
	b.ReportAllocs()
	m := NewPermissionMatrix()
	m.Grant("sre", "prod", ActionApply)
	m.Grant("sre", "prod", ActionPlan)
	m.Grant("sre", "staging", ActionAdmin)
	m.Grant("*", "dev", ActionView)
	m.Revoke("sre", "prod", ActionCancel)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Allow("sre", "prod", "apply")
	}
}

func BenchmarkPermissionMatrix_Allow_Wildcard(b *testing.B) {
	b.ReportAllocs()
	m := NewPermissionMatrix()
	m.Grant("*", "dev", ActionView)
	m.Grant("sre", "*", ActionPlan)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Allow("dba", "dev", "view")
	}
}

func BenchmarkPermissionMatrix_Allow_Admin(b *testing.B) {
	b.ReportAllocs()
	m := NewPermissionMatrix()
	m.Grant("sre", "prod", ActionAdmin)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Allow("sre", "prod", "apply")
	}
}

func BenchmarkPermissionMatrix_Allow_Denied(b *testing.B) {
	b.ReportAllocs()
	m := NewPermissionMatrix()
	m.Grant("sre", "prod", ActionApply)
	m.Revoke("sre", "prod", ActionApply)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Allow("sre", "prod", "apply")
	}
}

func BenchmarkPermissionMatrix_ActionsFor(b *testing.B) {
	b.ReportAllocs()
	m := NewPermissionMatrix()
	m.Grant("sre", "prod", ActionApply)
	m.Grant("sre", "prod", ActionPlan)
	m.Grant("sre", "prod", ActionAdmin)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.ActionsFor("sre", "prod")
	}
}

func BenchmarkPermissionMatrix_Teams(b *testing.B) {
	b.ReportAllocs()
	m := NewPermissionMatrix()
	for i := 0; i < 20; i++ {
		m.Grant(fmt.Sprintf("team-%02d", i), "prod", ActionApply)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Teams()
	}
}

func BenchmarkPermissionMatrix_Environments(b *testing.B) {
	b.ReportAllocs()
	m := NewPermissionMatrix()
	for i := 0; i < 10; i++ {
		m.Grant("sre", fmt.Sprintf("env-%02d", i), ActionApply)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Environments()
	}
}

func BenchmarkPermissionMatrix_LoadFromConfig(b *testing.B) {
	b.ReportAllocs()
	cfg := PermissionConfig{
		Teams: []TeamRule{
			{Name: "sre", Environments: []EnvPermission{
				{Name: "prod", Actions: []string{"plan", "apply", "rollback"}},
				{Name: "staging", Actions: []string{"plan", "apply", "admin"}},
			}},
			{Name: "dba", Environments: []EnvPermission{
				{Name: "prod", Actions: []string{"plan", "view"}},
			}},
		},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := NewPermissionMatrix()
		_ = m.LoadFromConfig(cfg)
	}
}

func BenchmarkPermissionMatrix_LoadFromJSON(b *testing.B) {
	b.ReportAllocs()
	data, _ := json.Marshal(PermissionConfig{
		Teams: []TeamRule{
			{Name: "sre", Environments: []EnvPermission{
				{Name: "prod", Actions: []string{"plan", "apply"}},
			}},
		},
	})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := NewPermissionMatrix()
		_ = m.LoadFromJSON(data)
	}
}

// --- PermissionChecker benchmarks ---

func BenchmarkPermissionChecker_Check(b *testing.B) {
	b.ReportAllocs()
	m := NewPermissionMatrix()
	m.Grant("sre", "prod", ActionApply)
	m.Grant("sre", "prod", ActionPlan)
	checker, _ := NewPermissionChecker(m)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = checker.Check(ctx, OperationContext{
			Actor: "alice", Team: "sre", Env: "prod", Action: ActionApply,
		})
	}
}

func BenchmarkPermissionChecker_CheckBatch(b *testing.B) {
	b.ReportAllocs()
	m := NewPermissionMatrix()
	m.Grant("sre", "prod", ActionApply)
	m.Grant("sre", "prod", ActionPlan)
	m.Grant("sre", "staging", ActionApply)
	checker, _ := NewPermissionChecker(m)
	ctx := context.Background()
	ops := []OperationContext{
		{Actor: "alice", Team: "sre", Env: "prod", Action: ActionPlan},
		{Actor: "alice", Team: "sre", Env: "prod", Action: ActionApply},
		{Actor: "alice", Team: "sre", Env: "staging", Action: ActionApply},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = checker.CheckBatch(ctx, ops...)
	}
}
