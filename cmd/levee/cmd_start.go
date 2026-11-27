package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/grpc/pb"
)

var (
	startOptForce          bool
	startOptMaxConcurrency int32
	startOptIdempotencyKey string
)

func init() {
	RegisterCommand(newStartCmd())
}

func newStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start <run-id>",
		Short: "Accept a change execution without waiting for its verdict",
		Long: "Start a planned change through the server's asynchronous StartChange RPC. " +
			"The response is an acceptance, not a terminal verdict: reconnect with GetChange " +
			"or WatchChange using the returned execution id (the change id). A durable " +
			"idempotency key is required; replaying it never starts a second execution. " +
			"This command requires --remote because a local CLI process would close the store " +
			"and exit while the asynchronous engine was still running. For local synchronous " +
			"execution, use apply --engine-enabled.",
		Args: cobra.ExactArgs(1),
		RunE: runStart,
	}
	cmd.Flags().BoolVar(&startOptForce, "force", false, "Skip approval check and force execution")
	cmd.Flags().Int32Var(&startOptMaxConcurrency, "max-concurrency", 0, "Override the plan's per-batch max concurrency (0 = use the plan)")
	cmd.Flags().StringVar(&startOptIdempotencyKey, "idempotency-key", "", "Durable retry key (required, maximum 128 bytes; reuse it after a disconnect)")
	_ = cmd.MarkFlagRequired("idempotency-key")
	return cmd
}

func runStart(cmd *cobra.Command, args []string) error {
	mode, err := resolveServiceMode()
	if err != nil {
		return err
	}
	if mode != modeRemote {
		return fmt.Errorf("start requires --remote: local asynchronous execution cannot outlive this CLI process; use apply --engine-enabled for local synchronous execution [exit=2]")
	}
	factory, err := newServiceFactory(mode, optServer, optAPIToken)
	if err != nil {
		return err
	}
	defer func() { _ = factory.Close() }()
	svc, err := factory.GetChangeService()
	if err != nil {
		return err
	}
	resp, err := svc.StartChange(agentRunContext(cmd), &pb.StartChangeRequest{
		ChangeId:       args[0],
		AutoApprove:    startOptForce,
		MaxConcurrency: startOptMaxConcurrency,
		IdempotencyKey: strings.TrimSpace(startOptIdempotencyKey),
	})
	if err != nil {
		return fmt.Errorf("start %s: %w [exit=%d]", args[0], err, startExitCode(err))
	}
	return printStartResponse(os.Stdout, resp)
}

func startExitCode(err error) int {
	switch grpcstatus.Code(err) {
	case codes.InvalidArgument:
		return 2
	case codes.FailedPrecondition, codes.AlreadyExists:
		return 4
	default:
		return 1
	}
}

func printStartResponse(w io.Writer, resp *pb.StartChangeResponse) error {
	data := map[string]any{
		"run_id":       resp.GetExecutionId(),
		"execution_id": resp.GetExecutionId(),
		"status":       resp.GetStatus(),
		"duplicate":    resp.GetDuplicate(),
		"message":      resp.GetMessage(),
	}
	if optJSON {
		return PrintJSON(w, map[string]any{"data": data, "meta": nil, "error": nil})
	}
	if optQuiet {
		fmt.Fprintln(w, resp.GetExecutionId())
		return nil
	}
	if resp.GetDuplicate() {
		fmt.Fprintf(w, "Run %s replayed the idempotency key; no new execution started (status=%s)\n",
			resp.GetExecutionId(), resp.GetStatus())
	} else {
		fmt.Fprintf(w, "Run %s execution accepted (status=%s)\n", resp.GetExecutionId(), resp.GetStatus())
	}
	fmt.Fprintln(w, "Acceptance is not a terminal verdict; reconnect with GetChange or WatchChange (execution_id is the change id).")
	return nil
}
