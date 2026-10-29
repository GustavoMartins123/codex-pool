package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type consentReader func([]byte) (int, error)

func (r consentReader) Read(p []byte) (int, error) { return r(p) }

func TestInstallConsentRequiresAnExplicitYes(t *testing.T) {
	for _, input := range []string{"y\n", "Y\n", "n\n", "\n", "", "yes\n"} {
		consent, err := readInstallConsent(context.Background(), strings.NewReader(input))
		want := input == "y\n" || input == "Y\n"
		if err != nil || consent != want {
			t.Errorf("consent for %q = %t, %v", input, consent, err)
		}
	}
}

func TestInstallConsentStopsWhenTheRunnerIsCancelled(t *testing.T) {
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	reading := make(chan struct{})
	reader := consentReader(func(p []byte) (int, error) {
		close(reading)
		return input.Read(p)
	})
	go func() {
		consent, err := readInstallConsent(ctx, reader)
		if consent {
			done <- errors.New("cancelled prompt granted consent")
			return
		}
		done <- err
	}()
	select {
	case <-reading:
	case <-time.After(time.Second):
		t.Fatal("prompt did not start reading input")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled prompt returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled prompt is still waiting for input")
	}
}

func TestCancelledAgentLookupDoesNotPromptForInstallation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := agents(ctx, t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled agent lookup returned %v", err)
	}
}
