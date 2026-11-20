package main

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestAccountRefreshCoordinatorSpacesAttemptsAndCancelsWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var coordinator accountRefreshCoordinator
		start := time.Now()
		if err := coordinator.waitForSlot(context.Background()); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := coordinator.waitForSlot(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled wait returned %v", err)
		}
		if !coordinator.lastAttempt.Equal(start) {
			t.Fatalf("cancelled wait consumed the refresh slot at %s", coordinator.lastAttempt)
		}

		if err := coordinator.waitForSlot(context.Background()); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != refreshMinInterval {
			t.Fatalf("second refresh waited %v, want %v", elapsed, refreshMinInterval)
		}

		var idle accountRefreshCoordinator
		if err := idle.waitForSlot(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled wait with a free slot returned %v", err)
		}
		if !idle.lastAttempt.IsZero() {
			t.Fatalf("cancelled wait consumed an idle refresh slot at %s", idle.lastAttempt)
		}
	})
}
