package main

import (
	"context"
	"testing"
	"time"
)

func TestOpContextZeroHasNoDeadline(t *testing.T) {
	ctx, cancel := opContext(context.Background(), 0)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Error("opContext(0) should not impose a deadline")
	}
}

func TestOpContextPositiveHasDeadline(t *testing.T) {
	ctx, cancel := opContext(context.Background(), time.Second)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Error("opContext(time.Second) should impose a deadline")
	}
}
