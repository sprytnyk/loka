package desktop

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
)

func TestDecodePaths(t *testing.T) {
	a := filepath.Join(t.TempDir(), "line\nbreak ")
	out, _ := json.Marshal([]string{a})
	if got, err := decodePaths(append(out, "\r\n"...)); err != nil || !slices.Equal(got, []string{a}) {
		t.Fatalf("got %q %v", got, err)
	}
	if got, err := decodePaths([]byte("[]")); err != nil || len(got) != 0 {
		t.Fatalf("empty answer gave %q %v", got, err)
	}
	for _, bad := range []string{`["relative"]`, "not json"} {
		if _, err := decodePaths([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}
