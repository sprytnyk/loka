package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sprytnyk/loka/internal/transfer"
	"github.com/sprytnyk/loka/internal/trust"
)

// tryOffer offers a file from a Loka that commits to nonce, and returns
// the reply or why it was refused.
func tryOffer(t *testing.T, client *http.Client, base, nonce string) (offerReply, error) {
	t.Helper()
	var reply offerReply
	o := offer{Name: "laptop", Items: []string{"f"}, Size: 1, Commitment: trust.Commit(nonce)}
	err := call(t.Context(), client, http.MethodPost, secure(base)+"/peer/offer", "", o, &reply)
	return reply, err
}

// TestDeviceInTheMiddleCantKeepTrying plays a device in the middle that
// offers again and again, to find a receiver's code that matches the
// sender's, and checks it soon runs out of tries and the person is warned.
func TestDeviceInTheMiddleCantKeepTrying(t *testing.T) {
	receiver, base := startServer(t)
	client := lokaClient(openTrust(t))
	for i := range pairingTries {
		reply, err := tryOffer(t, client, base, trust.Nonce())
		if err != nil {
			t.Fatalf("offer %d: %v", i, err)
		}
		receiver.Transfers.Get(reply.ID).Stop() // gives up, as the device in the middle would
	}
	if receiver.Warning() != "" {
		t.Fatal("warned before any offer was refused")
	}
	if _, err := tryOffer(t, client, base, trust.Nonce()); err == nil || err.Error() != errPairingLimit.Error() {
		t.Fatalf("offer after %d tries answered %v", pairingTries, err)
	}
	if receiver.Warning() != errPairingWarning.Error() {
		t.Fatalf("warning %q", receiver.Warning())
	}
}

// TestAcceptingGivesTheTryBack sends more transfers than there are tries,
// each accepted, and checks none is refused.
func TestAcceptingGivesTheTryBack(t *testing.T) {
	receiver, base := startServer(t)
	client := lokaClient(openTrust(t))
	for i := range pairingTries + 3 {
		nonce := trust.Nonce()
		reply, err := tryOffer(t, client, base, nonce)
		if err != nil {
			t.Fatalf("offer %d: %v", i, err)
		}
		if err := reveal(t, client, base, reply, nonce); err != nil {
			t.Fatal(err)
		}
		if !receiver.Accept(receiver.Transfers.Get(reply.ID), t.TempDir()) {
			t.Fatal("could not accept")
		}
	}
}

func TestTriesComeBackSlowly(t *testing.T) {
	var b pairingBudget
	now := time.Now()
	for range pairingTries {
		b.take(now)
	}
	if b.take(now.Add(pairingRefill - time.Second)) {
		t.Fatal("a try came back too soon")
	}
	if !b.take(now.Add(pairingRefill)) || b.take(now.Add(pairingRefill)) {
		t.Fatal("not exactly one try came back")
	}
}

// TestUnrevealedOfferExpires checks an offer whose sender never reveals
// fails, so it stops counting as waiting.
func TestUnrevealedOfferExpires(t *testing.T) {
	defer func(d time.Duration) { pairingTimeout = d }(pairingTimeout)
	pairingTimeout = 50 * time.Millisecond
	receiver, base := startServer(t)
	reply, err := tryOffer(t, lokaClient(openTrust(t)), base, trust.Nonce())
	if err != nil {
		t.Fatal(err)
	}
	if v := waitForEnd(t, receiver.Transfers.Get(reply.ID)); v.Status != transfer.Failed || v.Error != errPairingTimeout.Error() {
		t.Fatalf("offer ended %+v", v)
	}
}

// TestTooManyOffersWait fills the offers that may wait and checks the next
// is refused.
func TestTooManyOffersWait(t *testing.T) {
	receiver, base := startServer(t)
	o := offer{Items: []string{"f"}, Size: 1}
	for i := range maxOffers {
		if err := call(t.Context(), browser(receiver), http.MethodPost, secure(base)+"/peer/offer", "", o, nil); err != nil {
			t.Fatalf("offer %d: %v", i, err)
		}
	}
	err := call(t.Context(), browser(receiver), http.MethodPost, secure(base)+"/peer/offer", "", o, nil)
	if err == nil || err.Error() != errTooManyOffers.Error() {
		t.Fatalf("offer %d answered %v", maxOffers+1, err)
	}
}

// TestOfferNeedsARealCommitment offers without a commitment and with ones
// no Loka makes, and checks each is refused before the receiver keeps
// anything or spends a pairing try.
func TestOfferNeedsARealCommitment(t *testing.T) {
	receiver, base := startServer(t)
	client := lokaClient(openTrust(t))
	for _, commitment := range []string{"", "abc", strings.Repeat("a", 1<<19)} {
		o := offer{Name: "laptop", Items: []string{"f"}, Size: 1, Commitment: commitment}
		err := call(t.Context(), client, http.MethodPost, secure(base)+"/peer/offer", "", o, nil)
		if err == nil || err.Error() != "Invalid offer" {
			t.Fatalf("a commitment of %d characters answered %v", len(commitment), err)
		}
	}
	if n := len(receiver.Transfers.All()); n != 0 {
		t.Fatalf("the receiver keeps %d transfers", n)
	}
	if receiver.pairings.used("") != 0 {
		t.Fatalf("spent %d pairing tries", receiver.pairings.used(""))
	}
}

// TestRememberedDevicesHaveTheirOwnTries uses up the tries of new devices,
// and checks a remembered device still gets through. Then that device
// keeps trying, as a device in the middle would, and checks it runs out
// of its own tries without holding off another remembered device.
func TestRememberedDevicesHaveTheirOwnTries(t *testing.T) {
	receiver, base := startServer(t)
	giveUp := func(client *http.Client) error {
		reply, err := tryOffer(t, client, base, trust.Nonce())
		if err == nil {
			receiver.Transfers.Get(reply.ID).Stop()
		}
		return err
	}
	middle := lokaClient(openTrust(t))
	for range pairingTries {
		if err := giveUp(middle); err != nil {
			t.Fatal(err)
		}
	}
	if err := giveUp(middle); err == nil || err.Error() != errPairingLimit.Error() {
		t.Fatalf("a new device after the tries ran out got %v", err)
	}

	remembered := func(name string) *http.Client {
		store := openTrust(t)
		if err := receiver.Trust.Remember(trust.Device{ID: store.ID, Name: name, Fingerprint: store.Fingerprint()}); err != nil {
			t.Fatal(err)
		}
		return lokaClient(store)
	}
	guest := remembered("guest laptop") // renamed "laptop" once accepted
	for i := range pairingTries + 3 {
		nonce := trust.Nonce()
		reply, err := tryOffer(t, guest, base, nonce)
		if err != nil {
			t.Fatalf("remembered device, offer %d: %v", i, err)
		}
		if err := reveal(t, guest, base, reply, nonce); err != nil {
			t.Fatal(err)
		}
		if !receiver.Accept(receiver.Transfers.Get(reply.ID), t.TempDir()) {
			t.Fatal("couldn't accept")
		}
		receiver.Transfers.Get(reply.ID).Stop()
	}
	if n := receiver.pairings.used(""); n != pairingTries {
		t.Fatalf("new devices have %d tries used, want %d", n, pairingTries)
	}

	for i := range pairingTries {
		if err := giveUp(guest); err != nil {
			t.Fatalf("remembered device giving up, offer %d: %v", i, err)
		}
	}
	if err := giveUp(guest); err == nil || err.Error() != errPairingLimitKnown.Error() {
		t.Fatalf("a remembered device that kept trying got %v", err)
	}
	if err := giveUp(remembered("desktop")); err != nil {
		t.Fatalf("another remembered device got %v", err)
	}
	receiver.pairings.mu.Lock()
	delete(receiver.pairings.budgets, "")
	receiver.pairings.mu.Unlock()
	if got, want := receiver.Warning(), pairingWarning("laptop"); got != want {
		t.Fatalf("warning %q, want %q", got, want)
	}
}

// TestUnansweredOffersExpire fills every place for a waiting offer from
// browsers, which anyone can do, and checks the offers fail once nobody
// answers them, so others get through again.
func TestUnansweredOffersExpire(t *testing.T) {
	defer func(d time.Duration) { answerTimeout = d }(answerTimeout)
	answerTimeout = 100 * time.Millisecond
	receiver, base := startServer(t)
	o := offer{Items: []string{"f"}, Size: 1}
	for i := range maxOffers {
		if err := call(t.Context(), browser(receiver), http.MethodPost, secure(base)+"/peer/offer", "", o, nil); err != nil {
			t.Fatalf("offer %d: %v", i, err)
		}
	}
	for _, tr := range receiver.Transfers.All() {
		if v := waitForEnd(t, tr); v.Status != transfer.Failed || v.Error != errNoAnswer.Error() {
			t.Fatalf("offer ended %s, %q", v.Status, v.Error)
		}
	}
	if err := call(t.Context(), browser(receiver), http.MethodPost, secure(base)+"/peer/offer", "", o, nil); err != nil {
		t.Fatalf("an offer after the others expired got %v", err)
	}
}

// TestSenderLearnsNobodyAnswered sends to a Loka where nobody answers, and
// checks the sender says so rather than that the offer was declined.
func TestSenderLearnsNobodyAnswered(t *testing.T) {
	defer func(d time.Duration) { answerTimeout = d }(answerTimeout)
	answerTimeout = 100 * time.Millisecond
	receiver, host, port, _ := startRecorded(t)
	out := sendFrom(t, openTrust(t), []string{oneFile(t)}, host, port)
	if v := waitForEnd(t, out); v.Status != transfer.Failed || v.Error != errNoAnswer.Error() {
		t.Fatalf("the sender ended %s, %q", v.Status, v.Error)
	}
	if v := waitForEnd(t, waitForIncoming(t, receiver)); v.Error != errNoAnswer.Error() {
		t.Fatalf("the receiver ended %s, %q", v.Status, v.Error)
	}
}

// used is how many tries the budget at key has spent, for the tests.
func (l *pairingLimits) used(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.budgets[key]; b != nil {
		return b.used
	}
	return 0
}
