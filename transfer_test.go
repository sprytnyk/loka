package main

import (
	"errors"
	"sync"
	"testing"
)

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
