package main

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSpeedometer(t *testing.T) {
	var s speedometer
	start := time.Unix(1000, 0)
	for _, step := range []struct {
		bytes int64
		after time.Duration
		want  float64
	}{
		{0, 0, 0},                               // the first look only starts measuring
		{1_000_000, 250 * time.Millisecond, 0},  // too soon to measure
		{2_000_000, 500 * time.Millisecond, 32}, // 2 MB in 0.5 s
		{2_500_000, 750 * time.Millisecond, 32}, // too soon, keeps the last speed
		{2_500_000, 1500 * time.Millisecond, 4}, // 0.5 MB in 1 s
		{2_500_000, 2000 * time.Millisecond, 0}, // stalled
	} {
		if got := s.read(step.bytes, start.Add(step.after)); got != step.want {
			t.Fatalf("after %v: got %v Mbps, want %v", step.after, got, step.want)
		}
	}
}

func TestViewShowsSpeedOnlyWhileBytesMove(t *testing.T) {
	tr := newTransfer(outgoing, "receiver", nil, 10, statusWaiting)
	if v := tr.view(); v.SpeedMbps != nil {
		t.Fatalf("waiting transfer has speed %v", *v.SpeedMbps)
	}
	tr.move(statusSending, statusWaiting)
	tr.done.Add(10)
	if v := tr.view(); v.SpeedMbps == nil || *v.SpeedMbps != 0 {
		t.Fatalf("new send has speed %v", v.SpeedMbps)
	}
	// Measuring starts when sending starts, so the first look after
	// speedEvery already has a speed.
	time.Sleep(speedEvery)
	if v := tr.view(); v.SpeedMbps == nil || *v.SpeedMbps <= 0 {
		t.Fatalf("moving send has speed %v", v.SpeedMbps)
	}
	tr.finish(nil)
	if v := tr.view(); v.SpeedMbps != nil {
		t.Fatalf("finished transfer has speed %v", *v.SpeedMbps)
	}
}

func TestOnlyOneActorWinsAStep(t *testing.T) {
	tr := newTransfer(incoming, "sender", nil, 10, statusWaiting)
	var wins sync.WaitGroup
	results := make(chan bool, 50)
	for range 50 {
		wins.Go(func() { results <- tr.move(statusReceiving, statusAccepted, statusWaiting) })
	}
	wins.Wait()
	close(results)
	won := 0
	for ok := range results {
		if ok {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d actors moved the transfer, want 1", won)
	}
}

func TestAcceptOnlyWhileWaiting(t *testing.T) {
	tr := newTransfer(incoming, "sender", nil, 10, statusWaiting)
	tr.stop()
	if tr.accept("/tmp") {
		t.Fatal("accepted a declined transfer")
	}
	if got := tr.Status(); got != statusCancelled {
		t.Fatalf("status %q, want cancelled", got)
	}
	if tr.ctx.Err() == nil {
		t.Fatal("declining did not cancel the context")
	}
}

func TestFinishKeepsTheFirstEnding(t *testing.T) {
	tr := newTransfer(outgoing, "receiver", nil, 10, statusSending)
	tr.finish(errors.New("connection lost"))
	tr.finish(nil)
	v := tr.view()
	if v.Status != statusFailed || v.Error != "connection lost" {
		t.Fatalf("got %q %q, want failed with the first error", v.Status, v.Error)
	}
}

func TestFinishAfterStopIsCancelled(t *testing.T) {
	tr := newTransfer(outgoing, "receiver", nil, 10, statusSending)
	tr.stop()
	tr.finish(errors.New("read tcp: use of closed connection"))
	if v := tr.view(); v.Status != statusCancelled || v.Error != "" {
		t.Fatalf("got %q %q, want cancelled without an error", v.Status, v.Error)
	}
}

func TestStopEndsALink(t *testing.T) {
	tr := newTransfer(outgoing, "Link", nil, 0, statusShared)
	tr.stop()
	if got := tr.Status(); got != statusStopped {
		t.Fatalf("status %q, want stopped", got)
	}
}

func TestProgress(t *testing.T) {
	for _, c := range []struct {
		done, size int64
		status     Status
		want       int
	}{
		{0, 0, statusSending, 0},
		{0, 0, statusComplete, 100},
		{5, 10, statusSending, 50},
		{15, 10, statusReceiving, 100},
		{0, 0, statusShared, 100},
	} {
		if got := progress(c.done, c.size, c.status); got != c.want {
			t.Errorf("progress(%d, %d, %s) = %d, want %d", c.done, c.size, c.status, got, c.want)
		}
	}
}

func TestRegistryFindsLinksByToken(t *testing.T) {
	var r registry
	link := newTransfer(outgoing, "Link", nil, 0, statusShared)
	link.token = "abc"
	send := newTransfer(outgoing, "receiver", nil, 0, statusWaiting)
	r.add(link)
	r.add(send)
	if r.link("abc") != link || r.get(send.ID) != send || r.link("") != nil {
		t.Fatal("lookup returned the wrong transfer")
	}
	if all := r.all(); len(all) != 2 || all[0] != link {
		t.Fatal("transfers are not kept in creation order")
	}
}
