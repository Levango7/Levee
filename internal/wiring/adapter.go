// adapter.go exposes the assembled Engine through the gRPC EngineAdapter
// seam: Plan generates and hands back the persisted plan artifact; Run /
// Retry / Rollback execute the stored plan synchronously through the
// closure machinery (see run.go).

package wiring

import (
	"github.com/nexus/levee/internal/grpc"
)

// Adapter returns the EngineAdapter wiring this Engine into a
// ChangeService.
func (e *Engine) Adapter() *grpc.EngineAdapter {
	return &grpc.EngineAdapter{
		Plan:     e.GeneratePlan,
		Run:      e.runChange,
		Retry:    e.retryChange,
		Rollback: e.rollbackChange,
	}
}
