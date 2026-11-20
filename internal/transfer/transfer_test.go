package transfer

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
	tr := New(Outgoing, "receiver", nil, 10, Waiting)
	if v := tr.View(); v.SpeedMbps != nil {
		t.Fatalf("waiting transfer has speed %v", *v.SpeedMbps)
	}
	tr.Move(Sending, Waiting)
	tr.Done.Add(10)
	if v := tr.View(); v.SpeedMbps == nil || *v.SpeedMbps != 0 {
		t.Fatalf("new send has speed %v", v.SpeedMbps)
	}
	// Measuring starts when sending starts, so the first look after
	// speedEvery already has a speed.
	time.Sleep(speedEvery)
	if v := tr.View(); v.SpeedMbps == nil || *v.SpeedMbps <= 0 {
		t.Fatalf("moving send has speed %v", v.SpeedMbps)
	}
	tr.Finish(nil)
	if v := tr.View(); v.SpeedMbps != nil {
		t.Fatalf("finished transfer has speed %v", *v.SpeedMbps)
	}
}

func TestOnlyOneActorWinsAStep(t *testing.T) {
	tr := New(Incoming, "sender", nil, 10, Waiting)
	var wins sync.WaitGroup
	results := make(chan bool, 50)
	for range 50 {
		wins.Go(func() { results <- tr.Move(Receiving, Accepted, Waiting) })
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
	tr := New(Incoming, "sender", nil, 10, Waiting)
	tr.Stop()
	if tr.Accept("/tmp") {
		t.Fatal("accepted a declined transfer")
	}
	if got := tr.Status(); got != Cancelled {
		t.Fatalf("status %q, want cancelled", got)
	}
	if tr.ctx.Err() == nil {
		t.Fatal("declining did not cancel the context")
	}
}

func TestFinishKeepsTheFirstEnding(t *testing.T) {
	tr := New(Outgoing, "receiver", nil, 10, Sending)
	tr.Finish(errors.New("connection lost"))
	tr.Finish(nil)
	v := tr.View()
	if v.Status != Failed || v.Error != "connection lost" {
		t.Fatalf("got %q %q, want failed with the first error", v.Status, v.Error)
	}
}

func TestFinishAfterStopIsCancelled(t *testing.T) {
	tr := New(Outgoing, "receiver", nil, 10, Sending)
	tr.Stop()
	tr.Finish(errors.New("read tcp: use of closed connection"))
	if v := tr.View(); v.Status != Cancelled || v.Error != "" {
		t.Fatalf("got %q %q, want cancelled without an error", v.Status, v.Error)
	}
}

func TestStopEndsALink(t *testing.T) {
	tr := New(Outgoing, "Link", nil, 0, Shared)
	tr.Stop()
	if got := tr.Status(); got != Stopped {
		t.Fatalf("status %q, want stopped", got)
	}
}

func TestProgress(t *testing.T) {
	for _, c := range []struct {
		done, size int64
		status     Status
		want       int
	}{
		{0, 0, Sending, 0},
		{0, 0, Complete, 100},
		{5, 10, Sending, 50},
		{15, 10, Receiving, 100},
		{0, 0, Shared, 100},
	} {
		if got := progress(c.done, c.size, c.status); got != c.want {
			t.Errorf("progress(%d, %d, %s) = %d, want %d", c.done, c.size, c.status, got, c.want)
		}
	}
}

func TestRegistryFindsLinksByToken(t *testing.T) {
	var r Registry
	link := New(Outgoing, "Link", nil, 0, Shared)
	link.Token = "abc"
	send := New(Outgoing, "receiver", nil, 0, Waiting)
	r.Add(link)
	r.Add(send)
	if r.Link("abc") != link || r.Get(send.ID) != send || r.Link("") != nil {
		t.Fatal("lookup returned the wrong transfer")
	}
	if all := r.All(); len(all) != 2 || all[0] != link {
		t.Fatal("transfers are not kept in creation order")
	}
}

func TestRegistryDropsTheOldestEndedTransfers(t *testing.T) {
	var r Registry
	running := New(Incoming, "sender", nil, 0, Waiting)
	r.Add(running)
	var ended []*Transfer
	for range MaxEnded + 5 {
		e := New(Outgoing, "Link", nil, 0, Stopped)
		e.Token = RandomToken(9)
		ended = append(ended, e)
		r.Add(e)
	}
	if got := len(r.All()); got != MaxEnded+1 {
		t.Fatalf("kept %d transfers, want %d", got, MaxEnded+1)
	}
	if r.Get(running.ID) != running {
		t.Fatal("dropped a transfer that hasn't ended")
	}
	if r.Get(ended[4].ID) != nil || r.Link(ended[4].Token) != nil || r.Get(ended[5].ID) != ended[5] {
		t.Fatal("didn't drop exactly the oldest ended transfers")
	}
}
