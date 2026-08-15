// Package grpc shared helpers for pagination and common conversions.
package grpc

import (
	"fmt"
	"strconv"
)

// Pagination defaults.
const (
	defaultPageSize = 50
	maxPageSize     = 1000
)

// parsePageToken decodes a page token into a numeric offset. An empty or
// invalid token yields 0, so the caller starts from the beginning.
func parsePageToken(token string) int {
	if token == "" {
		return 0
	}
	n, err := strconv.Atoi(token)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// buildPageToken returns the next page token (the end offset) or an empty
// string when there are no more results.
func buildPageToken(end, total int) string {
	if end >= total {
		return ""
	}
	return fmt.Sprintf("%d", end)
}
