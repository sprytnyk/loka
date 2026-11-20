package trust

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// A pairing code lets two people check that their Lokas talk to each
// other and not to a device in the middle. Each side picks a random
// nonce. The sender commits to its nonce in the offer, the receiver
// answers with its own, and only then does the sender reveal its nonce.
// Both sides make the code from the two nonces and the two certificates
// they saw. A device in the middle has to fix one side's nonce before it
// sees the other side's, so it can't make both codes match, except by
// one chance in a million.

const nonceSize = 32

// Nonce returns a new random nonce, in hex.
func Nonce() string {
	b := make([]byte, nonceSize)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// CommitmentSize is the length of what Commit returns.
const CommitmentSize = 2 * sha256.Size

// Commit returns the commitment to a nonce, which the sender sends first.
func Commit(nonce string) string {
	sum := sha256.Sum256([]byte("loka commit\x00" + nonce))
	return hex.EncodeToString(sum[:])
}

// Code is the pairing code both sides show, made from the sender's and
// the receiver's certificate fingerprints and nonces.
func Code(sender, receiver, senderNonce, receiverNonce string) string {
	sum := sha256.Sum256([]byte("loka code\x00" + sender + "\x00" + receiver + "\x00" + senderNonce + "\x00" + receiverNonce))
	n := binary.BigEndian.Uint64(sum[:8]) % 1_000_000
	return fmt.Sprintf("%03d %03d", n/1000, n%1000)
}
