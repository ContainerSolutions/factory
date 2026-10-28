package controllers

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/memory"
)

// leaderPair is two instances' Leaders on one store (memory's locks are store-wide, as the
// object store's are across instances).
func leaderPair(t *testing.T) (*clock.Fake, *Leader, *Leader) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC))
	st := memory.New(store.Options{Clock: clk, Logger: discardLogger()})
	opts := LeaderOptions{TTL: 3 * time.Minute, Renew: time.Minute, Try: 30 * time.Millisecond, Clock: clk, Logger: discardLogger()}
	return clk, NewLeader(st, opts), NewLeader(st, opts)
}

func TestLeaderOneAtATime(t *testing.T) {
	ctx := context.Background()
	clk, a, b := leaderPair(t)
	a.step(ctx)
	b.step(ctx)
	if !a.Held() || b.Held() {
		t.Fatalf("held: a %v, b %v; want a only", a.Held(), b.Held())
	}
	// The holder renews; the other keeps trying and failing.
	clk.Advance(time.Minute)
	a.step(ctx)
	b.step(ctx)
	if !a.Held() || b.Held() {
		t.Fatalf("after a renewal: a %v, b %v; want a only", a.Held(), b.Held())
	}
	// A release hands it over at the other's next try.
	a.Release()
	if a.Held() {
		t.Fatal("a still holds the lock after Release")
	}
	b.step(ctx)
	if !b.Held() {
		t.Fatal("b didn't take the lock after a released it")
	}
}

func TestLeaderPausedPastItsLease(t *testing.T) {
	ctx := context.Background()
	clk, a, b := leaderPair(t)
	a.step(ctx)
	ended := a.term
	// a is paused past its lease: its gate closes on its own clock, before any renewal.
	clk.Advance(3*time.Minute + time.Second)
	if a.Held() {
		t.Fatal("a holds the lock past its lease")
	}
	b.step(ctx)
	if !b.Held() {
		t.Fatal("b didn't take the expired lock")
	}
	// a resumes: its renewal fails and its term ends.
	a.step(ctx)
	if a.Held() {
		t.Fatal("a holds the lock after b took it")
	}
	select {
	case <-ended:
	default:
		t.Fatal("a's term didn't end when its renewal failed")
	}
}

func TestLeaderGatesReconciles(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, a, b := leaderPair(t)
	var calls atomic.Int32
	ctrl := NewController("gated", ReconcilerFunc(func(context.Context, string) (Result, error) {
		calls.Add(1)
		return Result{}, nil
	}), Options{Workers: 1, Logger: discardLogger()})
	if err := ctrl.SetGate(b); err != nil {
		t.Fatal(err)
	}
	a.step(ctx) // a leads: b's controller waits
	done := make(chan struct{})
	go func() { _ = ctrl.Run(ctx); close(done) }()
	ctrl.Enqueue("k")
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("reconciled %d times while another instance leads", n)
	}
	a.Release()
	b.step(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the key wasn't reconciled once this instance led")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := ctrl.SetGate(a); err == nil {
		t.Fatal("SetGate after Run succeeded")
	}
	cancel()
	<-done
}

func TestLeaderWhileFollowsTheTerms(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, a, b := leaderPair(t)
	terms := make(chan context.Context, 4)
	done := make(chan struct{})
	go func() {
		a.While(ctx, "loop", func(ctx context.Context) error {
			terms <- ctx
			<-ctx.Done()
			return nil
		})
		close(done)
	}()
	a.step(ctx)
	first := <-terms
	a.Release() // the term ends: the loop's context with it
	select {
	case <-first.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the loop's context outlived the term")
	}
	b.step(ctx)
	b.Release()
	a.step(ctx) // a second term runs the loop again
	select {
	case <-terms:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop didn't run again in the next term")
	}
	cancel()
	<-done
}
