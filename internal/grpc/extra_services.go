// extra_services.go provides RegisterExtraServices, a single entry point
// that registers the AlertService, DiagnosisService and
// ConversationService added in C5 (task 132) on an existing *grpc.Server.
//
// The function lives in a separate file (rather than in server.go) so
// that the C5 work does not modify server.go, which is owned by the
// earlier task that introduced the original five services. A subsequent
// integration task will wire RegisterExtraServices into NewServer.
//
// All three services are optional: passing nil for any of them installs
// the generated Unimplemented*Server stub so the server still advertises
// the methods (returning codes.Unimplemented when called).

package grpc

import (
	"log/slog"

	"github.com/nexus/levee/internal/grpc/pb"

	ggrpc "google.golang.org/grpc"
)

// ExtraServicesConfig bundles the optional service implementations that
// RegisterExtraServices installs. Every field is optional; nil fields
// fall back to the generated Unimplemented stubs.
type ExtraServicesConfig struct {
	// Alert is the AlertService implementation. When nil an
	// UnimplementedAlertServiceServer is registered.
	Alert pb.AlertServiceServer
	// Diagnosis is the DiagnosisService implementation. When nil an
	// UnimplementedDiagnosisServiceServer is registered.
	Diagnosis pb.DiagnosisServiceServer
	// Conversation is the ConversationService implementation. When nil
	// an UnimplementedConversationServiceServer is registered.
	Conversation pb.ConversationServiceServer
	// Logger is the structured logger used by the default service
	// constructors when Alert/Diagnosis/Conversation are nil but the
	// caller still wants real implementations built from the supplied
	// engines. Unused when the caller supplies pre-built services.
	Logger *slog.Logger
}

// RegisterExtraServices registers the AlertService, DiagnosisService and
// ConversationService on s. It is safe to call multiple times; later
// calls overwrite earlier registrations for the same service.
//
// The function does not modify s in any other way; in particular it does
// not touch the listener, interceptors or the original five services.
func RegisterExtraServices(s *ggrpc.Server, cfg ExtraServicesConfig) {
	alertSvc := cfg.Alert
	if alertSvc == nil {
		alertSvc = &pb.UnimplementedAlertServiceServer{}
	}
	pb.RegisterAlertServiceServer(s, alertSvc)

	diagSvc := cfg.Diagnosis
	if diagSvc == nil {
		diagSvc = &pb.UnimplementedDiagnosisServiceServer{}
	}
	pb.RegisterDiagnosisServiceServer(s, diagSvc)

	convSvc := cfg.Conversation
	if convSvc == nil {
		convSvc = &pb.UnimplementedConversationServiceServer{}
	}
	pb.RegisterConversationServiceServer(s, convSvc)
}
