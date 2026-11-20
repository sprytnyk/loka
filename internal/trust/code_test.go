package trust

import (
	"regexp"
	"testing"
)

// TestCode checks the code has six digits and changes when any of the
// certificates or nonces does.
func TestCode(t *testing.T) {
	in := [4]string{"sender", "receiver", Nonce(), Nonce()}
	code := Code(in[0], in[1], in[2], in[3])
	if !regexp.MustCompile(`^\d{3} \d{3}$`).MatchString(code) {
		t.Fatalf("code %q", code)
	}
	if again := Code(in[0], in[1], in[2], in[3]); again != code {
		t.Fatalf("the same inputs gave %q, then %q", code, again)
	}
	for i := range in {
		changed := in
		changed[i] += "x"
		if Code(changed[0], changed[1], changed[2], changed[3]) == code {
			t.Errorf("changing input %d kept the code", i)
		}
	}
}

func TestCommit(t *testing.T) {
	n := Nonce()
	if len(n) != 2*nonceSize || n == Nonce() {
		t.Fatalf("nonce %q", n)
	}
	c := Commit(n)
	if again := Commit(n); again != c || c == Commit(Nonce()) || c == n {
		t.Fatal("commitments don't follow their nonce")
	}
}
