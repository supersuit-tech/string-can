package server_test

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supersuit-tech/string-can/internal/server"
	"github.com/supersuit-tech/string-can/internal/store"
)

func TestRetryWakesFrozenPeer(t *testing.T) {
	t.Setenv("STRING_CAN_WAKE_WAIT", "2s")
	t.Setenv("STRING_CAN_WAKE_POLL", "10ms")
	t.Setenv("STRING_CAN_WAKE_INTERVAL", "2m")
	t.Setenv("STRING_CAN_PEER_TIMEOUT", "500ms")

	const (
		token   = "tick-secret-token"
		message = "tick-secret-message"
	)
	var up atomic.Bool
	var wakes atomic.Int32
	var wakeBody atomic.Value
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wakes.Add(1)
		b, _ := io.ReadAll(r.Body)
		wakeBody.Store(string(b))
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		up.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "down", http.StatusServiceUnavailable)
				return
			}
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"v":1,"name":"bot","public_key":"abcd"}`)
			return
		}
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"messages":[]}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(peer.Close)

	var logs bytes.Buffer
	home := t.TempDir()
	srv := newWakeServer(t, home, &logs)
	peerKey := strings.Repeat("ab", 32)
	if err := srv.Store().UpsertContact(store.Contact{
		PublicKey: peerKey,
		Label:     "bot",
		BaseURL:   peer.URL,
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store().SetWake(peerKey, hook.URL+"/path/"+token, token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := srv.Store().InsertMessage(store.Message{
		PeerPub:   peerKey,
		FromPub:   srv.PublicKey(),
		Ts:        1,
		Text:      message,
		Body:      `{"v":1,"text":"` + message + `"}`,
		Signature: "sig-wake",
	}); err != nil {
		t.Fatal(err)
	}
	srv.Tick(context.Background())
	if wakes.Load() != 1 {
		t.Fatalf("wakes %d logs %s", wakes.Load(), logs.String())
	}
	if msgs := undelivered(t, home); len(msgs) != 0 {
		t.Fatalf("still undelivered %+v logs %s", msgs, logs.String())
	}
	raw, _ := wakeBody.Load().(string)
	if strings.Contains(raw, message) || strings.Contains(raw, token) {
		t.Fatalf("wake body %s", raw)
	}
	if !strings.Contains(raw, `"type":"string-can.wake"`) || !strings.Contains(raw, srv.PublicKey()) {
		t.Fatalf("wake body %s", raw)
	}
	logged := logs.String()
	if strings.Contains(logged, token) || strings.Contains(logged, message) || strings.Contains(logged, "/path/") {
		t.Fatalf("logs leaked %s", logged)
	}
	if !strings.Contains(logged, "peer wake: posted") || !strings.Contains(logged, "delivered") {
		t.Fatalf("logs %s", logged)
	}
}

func newWakeServer(t *testing.T, home string, logs *bytes.Buffer) *server.Server {
	t.Helper()
	srv, err := server.New(server.Config{
		Home:      home,
		Name:      "muse",
		Port:      0,
		Local:     true,
		WakeEvery: time.Hour,
		Logger:    log.New(logs, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}
