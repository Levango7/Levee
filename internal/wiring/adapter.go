// adapter.go exposes the assembled Engine through the gRPC EngineAdapter
// seam. Plan is served immediately; the execution closures (Run / Retry /
// Rollback) are attached here as the execution path lands.

package wiring

import (
	"github.com/nexus/levee/internal/grpc"
)

// Adapter returns the EngineAdapter wiring this Engine into a
// ChangeService. The Plan seam is always present; Run/Retry/Rollback are
// attached by the execution wiring.
func (e *Engine) Adapter() *grpc.EngineAdapter {
	return &grpc.EngineAdapter{
		Plan: e.GeneratePlan,
	}
}
