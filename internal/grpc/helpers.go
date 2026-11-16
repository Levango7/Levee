// Package grpc shared helpers for pagination and common conversions.
package grpc

import (
	"fmt"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Pagination defaults.
const (
	defaultPageSize = 50
	maxPageSize     = 1000
)

// parsePageToken decodes a page token into a numeric offset. An empty
// token yields (0, nil) so the caller starts from the beginning. A
// malformed or negative token yields a codes.InvalidArgument status
// error: silently restarting from page 0 used to hide client bugs and
// made pagination loops re-fetch the same data forever.
func parsePageToken(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(token)
	if err != nil || n < 0 {
		return 0, status.Errorf(codes.InvalidArgument, "invalid page token %q", token)
	}
	return n, nil
}

// buildPageToken returns the next page token (the end offset) or an empty
// string when there are no more results.
func buildPageToken(end, total int) string {
	if end >= total {
		return ""
	}
	return fmt.Sprintf("%d", end)
}

// classifiedStatus returns the gRPC status carried by err, when err carries
// one.
//
// A refusal classified by a lower layer KEEPS its classification: the wiring
// layer knows whether a plan request was malformed (InvalidArgument → 400) or
// whether the state forbids it (FailedPrecondition → 412), and burying that
// in Internal turns an operator-actionable refusal into a server fault — the
// operator gets a 500 for a request that was simply wrong.
//
// Only a real classification passes: an ordinary error yields ok=false from
// status.FromError, and codes.Unknown is treated as "not classified" (that is
// what status.FromError reports for a plain error).
func classifiedStatus(err error) (*status.Status, bool) {
	if err == nil {
		return nil, false
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() == codes.Unknown {
		return nil, false
	}
	return st, true
}
