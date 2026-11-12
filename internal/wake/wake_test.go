package wake

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/supersuit-tech/string-can/internal/push"
	"github.com/supersuit-tech/string-can/internal/store"
)

func TestMaskURLHidesPathAndSecrets(t *testing.T) {
	token := "super-secret-token"
	raw := "https://user:" + token + "@hooks.example.com/v1/" + token + "?access_token=" + token
	got := MaskURL(raw)
	if got != "https://hooks.example.com/..." {
		t.Fatalf("mask %q", got)
	}
	if strings.Contains(got, token) || strings.Contains(got, "user") {
		t.Fatalf("mask leaked %q", got)
	}
	if MaskURL("") != "" {
		t.Fatal("empty")
	}
	if MaskURL("not a url") != "configured" {
		t.Fatal("non-url should not be echoed")
	}
	if MaskURL("https://hooks.example.com/") != "https://hooks.example.com/" {
		t.Fatalf("root %q", MaskURL("https://hooks.example.com/"))
	}
}

func TestNormalizeURLRejectsUserinfo(t *testing.T) {
	if _, err := NormalizeURL("https://user:secret@hooks.example.com/wake"); err == nil {
		t.Fatal("accepted userinfo")
	}
	got, err := NormalizeURL("https://hooks.example.com/wake")
	if err != nil || got != "https://hooks.example.com/wake" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestLoadTokenPrefersFileAndWarnsOnLiteral(t *testing.T) {
	t.Setenv("WAKE_TOKEN_TEST", "from-env")
	got, warn, err := LoadToken("", "WAKE_TOKEN_TEST", "")
	if err != nil || warn != "" || got != "from-env" {
		t.Fatalf("%q %q %v", got, warn, err)
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, warn, err = LoadToken(path, "", "")
	if err != nil || warn != "" || got != "from-file" {
		t.Fatalf("file %q %q %v", got, warn, err)
	}
	got, warn, err = LoadToken("", "", "from-arg")
	if err != nil || got != "from-arg" || !strings.Contains(warn, "shell history") {
		t.Fatalf("literal %q %q %v", got, warn, err)
	}
	if _, _, err := LoadToken(path, "WAKE_TOKEN_TEST", ""); err == nil {
		t.Fatal("two sources accepted")
	}
	if strings.Contains(warn, "from-arg") {
		t.Fatal("warning contains the token")
	}
}

func TestSendAndFetchNeedWake(t *testing.T) {
	if !SendNeedsWake(push.KindUnconfirmed, context.DeadlineExceeded) {
		t.Fatal("unconfirmed should wake")
	}
	if SendNeedsWake(push.KindPending, &push.HTTPError{Status: 500}) {
		t.Fatal("HTTP status means the peer answered")
	}
	if !SendNeedsWake(push.KindPending, context.DeadlineExceeded) {
		t.Fatal("transport failure should wake")
	}
	if !FetchNeedsWake(context.DeadlineExceeded) {
		t.Fatal("fetch transport")
	}
	if FetchNeedsWake(errPeer("peer returned 500")) {
		t.Fatal("fetch HTTP status")
	}
}

type errPeer string

func (e errPeer) Error() string { return string(e) }

func TestTryWakesThenSeesPeerAndDoesNotRepeat(t *testing.T) {
	t.Setenv("STRING_CAN_WAKE_WAIT", "2s")
	t.Setenv("STRING_CAN_WAKE_POLL", "10ms")
	t.Setenv("STRING_CAN_WAKE_INTERVAL", "2m")

	const (
		token   = "super-secret-token"
		message = "message-text-must-not-appear"
		from    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	var up atomic.Bool
	var mu sync.Mutex
	var body []byte
	var auth string
	var wakes atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wakes.Add(1)
		mu.Lock()
		auth = r.Header.Get("Authorization")
		body, _ = io.ReadAll(r.Body)
		mu.Unlock()
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
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"v":1,"name":"bot","public_key":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`)
	}))
	t.Cleanup(peer.Close)

	st := openWakeStore(t)
	peerKey := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := st.UpsertContact(store.Contact{PublicKey: peerKey, Label: "bot", BaseURL: peer.URL}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWake(peerKey, hook.URL+"/hooks/"+token, token); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	logf := func(format string, args ...any) {
		logs.WriteString(fmt.Sprintf(format, args...))
		logs.WriteByte('\n')
	}
	if !Try(context.Background(), st, peerKey, peer.URL, from, logf) {
		t.Fatalf("expected retry, logs %s", logs.String())
	}
	if wakes.Load() != 1 {
		t.Fatalf("wakes %d", wakes.Load())
	}
	mu.Lock()
	gotBody := string(body)
	gotAuth := auth
	mu.Unlock()
	if gotAuth != "Bearer "+token {
		t.Fatalf("auth %q", gotAuth)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["type"] != "string-can.wake" || payload["from"] != from || len(payload) != 2 {
		t.Fatalf("body %s", gotBody)
	}
	if strings.Contains(gotBody, message) || strings.Contains(gotBody, token) {
		t.Fatalf("wake body leaked %s", gotBody)
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), message) || strings.Contains(logs.String(), "/hooks/") {
		t.Fatalf("logs leaked %s", logs.String())
	}
	if !strings.Contains(logs.String(), "posted") || !strings.Contains(logs.String(), "is up") {
		t.Fatalf("logs %s", logs.String())
	}
	logs.Reset()
	if Try(context.Background(), st, peerKey, peer.URL, from, logf) {
		t.Fatal("second wake should be rate limited")
	}
	if wakes.Load() != 1 {
		t.Fatalf("wakes after limit %d", wakes.Load())
	}
	if !strings.Contains(logs.String(), "skipped") {
		t.Fatalf("logs %s", logs.String())
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), message) {
		t.Fatalf("limit logs leaked %s", logs.String())
	}
}

func TestTryWakeFailureDoesNotPanic(t *testing.T) {
	t.Setenv("STRING_CAN_WAKE_INTERVAL", "2m")
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusBadGateway)
	}))
	t.Cleanup(hook.Close)
	st := openWakeStore(t)
	peerKey := strings.Repeat("ab", 32)
	if err := st.UpsertContact(store.Contact{PublicKey: peerKey, Label: "bot", BaseURL: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	const token = "another-secret-token"
	if err := st.SetWake(peerKey, hook.URL, token); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	logf := func(format string, args ...any) {
		logs.WriteString(fmt.Sprintf(format, args...))
		logs.WriteByte('\n')
	}
	if Try(context.Background(), st, peerKey, "http://127.0.0.1:1", strings.Repeat("cd", 32), logf) {
		t.Fatal("failed wake should not ask for a retry")
	}
	if !strings.Contains(logs.String(), "post failed") {
		t.Fatalf("logs %s", logs.String())
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("logs leaked token %s", logs.String())
	}
}

func openWakeStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "string-can.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
