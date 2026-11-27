package grpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/permission"
	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
)

const maxStartIdempotencyKeyLen = 128

// StartChange accepts an execution and returns before the engine settles. The
// durable key is what makes an HTTP retry or a process restart a replay rather
// than a second execution. Existing ApplyChange remains the synchronous API.
func (s *ChangeService) StartChange(ctx context.Context, req *pb.StartChangeRequest) (*pb.StartChangeResponse, error) {
	key := strings.TrimSpace(req.GetIdempotencyKey())
	if key == "" {
		return nil, status.Error(codes.InvalidArgument, "idempotency_key is required for StartChange")
	}
	if len(key) > maxStartIdempotencyKeyLen {
		return nil, status.Errorf(codes.InvalidArgument, "idempotency_key exceeds %d bytes", maxStartIdempotencyKeyLen)
	}
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}
	run, err := s.store.GetRun(ctx, req.GetChangeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get run: %v", err)
	}
	if run == nil {
		return nil, status.Errorf(codes.NotFound, "change %q not found", req.GetChangeId())
	}
	if err := s.authorize(ctx, run, permission.ActionApply, "apply"); err != nil {
		return nil, err
	}
	idemStore, ok := s.store.(state.ChangeIdempotencyStore)
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "this store does not support durable StartChange idempotency")
	}
	fingerprint := startRequestFingerprint(req)
	existing, err := idemStore.GetChangeIdempotency(ctx, run.ID, key)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read idempotency key: %v", err)
	}
	if existing != nil {
		return startDuplicateResponse(run, existing, fingerprint)
	}

	record := &state.ChangeIdempotency{
		ChangeID:           run.ID,
		IdempotencyKey:     key,
		RequestFingerprint: fingerprint,
		CreatedAt:          time.Now().UTC(),
	}
	prepared, err := s.prepareApplyWithTransition(ctx, &pb.ApplyChangeRequest{
		ChangeId:       req.GetChangeId(),
		AutoApprove:    req.GetAutoApprove(),
		MaxConcurrency: req.GetMaxConcurrency(),
		DryRun:         req.GetDryRun(),
	}, func(ctx context.Context, id, from, to string, updatedAt time.Time) (bool, error) {
		already, transitioned, claimErr := idemStore.ClaimChangeExecution(ctx, record, from, to, updatedAt)
		if claimErr != nil {
			return false, fmt.Errorf("claim durable idempotency: %w", claimErr)
		}
		if already {
			return false, errApplyAlreadyClaimed
		}
		return transitioned, nil
	})
	if errors.Is(err, errApplyAlreadyClaimed) {
		return s.startClaimedResponse(ctx, run.ID, key, fingerprint)
	}
	if err != nil {
		return nil, err
	}

	// Build the response before the goroutine can settle and mutate the run;
	// otherwise the first asynchronous response itself becomes a data race.
	accepted := &pb.StartChangeResponse{
		Change:      runToPB(prepared.run),
		ExecutionId: run.ID,
		Status:      runstatus.StatusRunning,
		Duplicate:   false,
		Message:     "execution accepted; reconnect with GetChange or WatchChange for the terminal verdict",
	}
	executionCtx := context.WithoutCancel(ctx)
	go func() {
		// Values (actor, tenant and auth metadata) survive WithoutCancel, while a
		// client disconnect no longer cancels the execution it merely started.
		_, execErr := s.executeApply(executionCtx, prepared)
		if execErr != nil {
			log.Error("asynchronous apply failed", "run_id", run.ID, "error", execErr)
		}
	}()
	return accepted, nil
}

func (s *ChangeService) startClaimedResponse(ctx context.Context, changeID, key, fingerprint string) (*pb.StartChangeResponse, error) {
	idemStore := s.store.(state.ChangeIdempotencyStore)
	existing, err := idemStore.GetChangeIdempotency(ctx, changeID, key)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read claimed idempotency key: %v", err)
	}
	if existing == nil {
		return nil, status.Error(codes.Internal, "claimed idempotency key disappeared before replay")
	}
	run, err := s.store.GetRun(ctx, changeID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get claimed run: %v", err)
	}
	if run == nil {
		return nil, status.Errorf(codes.NotFound, "change %q not found", changeID)
	}
	return startDuplicateResponse(run, existing, fingerprint)
}

func startDuplicateResponse(run *state.Run, existing *state.ChangeIdempotency, fingerprint string) (*pb.StartChangeResponse, error) {
	if existing.RequestFingerprint != fingerprint {
		return nil, status.Error(codes.AlreadyExists, "idempotency key was already used with different execution parameters")
	}
	return &pb.StartChangeResponse{
		Change:      runToPB(run),
		ExecutionId: run.ID,
		Status:      run.Status,
		Duplicate:   true,
		Message:     "duplicate idempotency key: no new execution was started",
	}, nil
}

func startRequestFingerprint(req *pb.StartChangeRequest) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%t:%d:%t", req.GetAutoApprove(), req.GetMaxConcurrency(), req.GetDryRun())))
	return hex.EncodeToString(sum[:])
}
