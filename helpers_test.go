package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sprytnyk/loka/internal/transfer"
)

// startServer runs a server on a loopback port and returns it with its URL.
func startServer(t *testing.T) (*server, string) {
	t.Helper()
	s := newServer("receiver", "test")
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return s, ts.URL
}

// api calls the page API of the server at url and decodes the reply into
// reply when it is not nil. It returns the HTTP status.
func api(t *testing.T, s *server, url, path string, body, reply any) int {
	t.Helper()
	method := http.MethodGet
	var payload io.Reader
	if body != nil {
		method = http.MethodPost
		data, _ := json.Marshal(body)
		payload = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, url+path, payload)
	req.Header.Set("X-Loka-Key", s.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if reply != nil {
		json.NewDecoder(resp.Body).Decode(reply)
	}
	return resp.StatusCode
}

// waitFor polls until cond is true or a few seconds pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForIncoming returns the first incoming transfer on s.
func waitForIncoming(t *testing.T, s *server) *transfer.Transfer {
	t.Helper()
	var found *transfer.Transfer
	waitFor(t, "an incoming offer", func() bool {
		for _, tr := range s.transfers.All() {
			if tr.Direction == transfer.Incoming {
				found = tr
				return true
			}
		}
		return false
	})
	return found
}

func waitForEnd(t *testing.T, tr *transfer.Transfer) transfer.View {
	t.Helper()
	waitFor(t, "the transfer to end", func() bool { return tr.Status().Final() })
	return tr.View()
}
