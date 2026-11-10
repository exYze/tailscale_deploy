//go:build !darwin

package main

import (
	"context"
	"time"
)

func runCLI(ctx context.Context, timeout time.Duration, quiet bool, cli string, args ...string) (string, error) {
	return runCmd(ctx, timeout, quiet, cli, args...)
}
