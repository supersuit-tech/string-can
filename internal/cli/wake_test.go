package cli_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/supersuit-tech/string-can/internal/canon"
	"github.com/supersuit-tech/string-can/internal/cli"
	"github.com/supersuit-tech/string-can/internal/store"
	"github.com/supersuit-tech/string-can/internal/wake"
)

func TestWakeConfigMasksToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("STRING_CAN_HOME", home)
	t.Setenv("STRING_CAN_WAKE_PROXY", "")
	const (
		token   = "config-secret-token"
		message = "do-not-log-this-message"
	)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"v":1,"name":"bot","public_key":"`+strings.Repeat("ab", 32)+`"}`)
	}))
	t.Cleanup(peer.Close)
	key := strings.Repeat("ab", 32)
	t.Setenv("GROK_WAKE_TOKEN", token)
	stdout, stderr, code := runCLI(t, "connect", "--local", "--name", "bot",
		"--wake-url", "https://hooks.example.com/v1/"+token,
		"--wake-token-env", "GROK_WAKE_TOKEN",
		key, peer.URL)
	if code != 0 {
		t.Fatalf("connect exit %d\n%s\n%s", code, stdout, stderr)
	}
	assertNoSecret(t, stdout+stderr, token, message)
	if !strings.Contains(stdout, wake.MaskedToken) || !strings.Contains(stdout, "https://hooks.example.com/...") {
		t.Fatalf("connect json %s", stdout)
	}
	if strings.Contains(stdout, "/v1/") {
		t.Fatalf("path leaked %s", stdout)
	}

	stdout, stderr, code = runCLI(t, "wake", "show", key)
	if code != 0 {
		t.Fatalf("show %d %s %s", code, stdout, stderr)
	}
	assertNoSecret(t, stdout+stderr, token, message)
	if !strings.Contains(stdout, `"wake_token":"`+wake.MaskedToken+`"`) {
		t.Fatalf("show %s", stdout)
	}

	stdout, stderr, code = runCLI(t, "contacts")
	if code != 0 {
		t.Fatalf("contacts %d %s", code, stderr)
	}
	assertNoSecret(t, stdout+stderr, token, message)
	if !strings.Contains(stdout, `"wake_url":"https://hooks.example.com/..."`) {
		t.Fatalf("contacts %s", stdout)
	}

	stdout, stderr, code = runCLI(t, "doctor")
	assertNoSecret(t, stdout+stderr, token, message)
	if !strings.Contains(stdout, "wake: https://hooks.example.com/... token "+wake.MaskedToken+" proxy env") {
		t.Fatalf("doctor %s", stdout)
	}
	if !strings.Contains(stdout, "wake proxy: env") {
		t.Fatalf("doctor proxy %s", stdout)
	}

	st, err := store.Open(filepath.Join(home, "string-can.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	gotURL, gotToken, _, err := st.LoadWake(key)
	if err != nil || gotToken != token || !strings.Contains(gotURL, token) {
		t.Fatalf("stored %q %q %v", gotURL, gotToken, err)
	}

	stdout, stderr, code = runCLI(t, "wake", "set", key, "https://hooks.example.com/renamed")
	if code != 0 {
		t.Fatalf("set url only %d %s %s", code, stdout, stderr)
	}
	assertNoSecret(t, stdout+stderr, token, message)
	gotURL, gotToken, _, err = st.LoadWake(key)
	if err != nil || gotToken != token || gotURL != "https://hooks.example.com/renamed" {
		t.Fatalf("url-only set %q %q %v", gotURL, gotToken, err)
	}

	stdout, stderr, code = runCLI(t, "wake", "set", key, "https://hooks.example.com/other/"+token, "--token", token)
	if code != 0 {
		t.Fatalf("literal set %d %s %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "shell history") {
		t.Fatalf("stderr %s", stderr)
	}
	assertNoSecret(t, stdout+stderr, token, message)

	stdout, stderr, code = runCLI(t, "wake", "clear", key)
	if code != 0 {
		t.Fatalf("clear %d %s %s", code, stdout, stderr)
	}
	if strings.Contains(stdout, wake.MaskedToken) || strings.Contains(stdout, "hooks.example.com") {
		t.Fatalf("clear %s", stdout)
	}
	_, gotToken, _, err = st.LoadWake(key)
	if err != nil || gotToken != "" {
		t.Fatalf("token after clear %q %v", gotToken, err)
	}
}

func TestWakeProxyMaskedInCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("STRING_CAN_HOME", home)
	t.Setenv("STRING_CAN_WAKE_PROXY", "")
	const (
		secret = "proxy-secret-password"
		user   = "proxyuser"
		token  = "config-secret-token"
	)
	proxyURL := "http://" + user + ":" + secret + "@proxy.example:3128"
	masked := "http://********@proxy.example:3128"
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"v":1,"name":"bot","public_key":"`+strings.Repeat("ab", 32)+`"}`)
	}))
	t.Cleanup(peer.Close)
	key := strings.Repeat("ab", 32)
	t.Setenv("GROK_WAKE_TOKEN", token)
	stdout, stderr, code := runCLI(t, "connect", "--local", "--name", "bot",
		"--wake-url", "https://hooks.example.com/v1/wake",
		"--wake-token-env", "GROK_WAKE_TOKEN",
		key, peer.URL)
	if code != 0 {
		t.Fatalf("connect %d %s %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `"wake_proxy":"env"`) {
		t.Fatalf("connect proxy %s", stdout)
	}

	stdout, stderr, code = runCLI(t, "wake", "show", key)
	if code != 0 || !strings.Contains(stdout, `"wake_proxy":"env"`) {
		t.Fatalf("show %d %s %s", code, stdout, stderr)
	}

	t.Setenv("STRING_CAN_WAKE_PROXY", proxyURL)
	stdout, stderr, code = runCLI(t, "wake", "show", key)
	assertProxyMasked(t, stdout+stderr, secret, user)
	if code != 0 || !strings.Contains(stdout, `"wake_proxy":"`+masked+`"`) {
		t.Fatalf("show env %d %s", code, stdout)
	}
	stdout, stderr, code = runCLI(t, "contacts")
	assertProxyMasked(t, stdout+stderr, secret, user)
	if !strings.Contains(stdout, `"wake_proxy":"`+masked+`"`) {
		t.Fatalf("contacts %s", stdout)
	}
	stdout, stderr, code = runCLI(t, "doctor")
	assertProxyMasked(t, stdout+stderr, secret, user)
	if !strings.Contains(stdout, "wake proxy: "+masked) || !strings.Contains(stdout, "proxy "+masked) {
		t.Fatalf("doctor %s", stdout)
	}

	stdout, stderr, code = runCLI(t, "wake", "set", key, "https://hooks.example.com/renamed", "--proxy", "direct")
	assertProxyMasked(t, stdout+stderr, secret, user)
	if code != 0 || !strings.Contains(stdout, `"wake_proxy":"direct"`) {
		t.Fatalf("set direct %d %s %s", code, stdout, stderr)
	}
	stdout, stderr, code = runCLI(t, "doctor")
	assertProxyMasked(t, stdout+stderr, secret, user)
	if !strings.Contains(stdout, "wake proxy: "+masked) || !strings.Contains(stdout, "proxy direct") {
		t.Fatalf("doctor override %s", stdout)
	}

	stdout, stderr, code = runCLI(t, "wake", "set", key, "https://hooks.example.com/nope", "--proxy", "ftp://"+user+":"+secret+"@proxy.example")
	assertProxyMasked(t, stdout+stderr, secret, user)
	if code == 0 {
		t.Fatalf("accepted bad proxy %s", stdout+stderr)
	}
	stdout, stderr, code = runCLI(t, "wake", "show", key)
	if !strings.Contains(stdout, `"wake_url":"https://hooks.example.com/..."`) || !strings.Contains(stdout, `"wake_proxy":"direct"`) {
		t.Fatalf("bad proxy wrote %s", stdout)
	}
	if strings.Contains(stdout, "/nope") {
		t.Fatalf("bad proxy changed url %s", stdout)
	}

	stdout, stderr, code = runCLI(t, "wake", "set", key, "https://hooks.example.com/again")
	if code != 0 || !strings.Contains(stdout, `"wake_proxy":"direct"`) {
		t.Fatalf("url-only %d %s %s", code, stdout, stderr)
	}

	stdout, stderr, code = runCLI(t, "wake", "set", key, "https://hooks.example.com/again", "--proxy", "env")
	assertProxyMasked(t, stdout+stderr, secret, user)
	if code != 0 || !strings.Contains(stdout, `"wake_proxy":"`+masked+`"`) {
		t.Fatalf("clear override %d %s %s", code, stdout, stderr)
	}

	stdout, stderr, code = runCLI(t, "wake", "set", key, "https://hooks.example.com/again", "--proxy", "http://"+user+":"+secret+"@alt.example:9")
	assertProxyMasked(t, stdout+stderr, secret, user)
	if code != 0 || !strings.Contains(stdout, `"wake_proxy":"http://********@alt.example:9"`) {
		t.Fatalf("contact url %d %s %s", code, stdout, stderr)
	}
	st, err := store.Open(filepath.Join(home, "string-can.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, _, gotProxy, err := st.LoadWake(key)
	if err != nil || !strings.Contains(gotProxy, secret) {
		t.Fatalf("stored proxy %q %v", gotProxy, err)
	}
}

func assertProxyMasked(t *testing.T, text, secret, user string) {
	t.Helper()
	if strings.Contains(text, secret) || strings.Contains(text, user) {
		t.Fatalf("output leaked proxy credentials: %s", text)
	}
}

func TestSendWakesFrozenPeerThenRetries(t *testing.T) {
	t.Setenv("STRING_CAN_WAKE_WAIT", "2s")
	t.Setenv("STRING_CAN_WAKE_POLL", "10ms")
	t.Setenv("STRING_CAN_WAKE_INTERVAL", "2m")
	t.Setenv("STRING_CAN_PEER_TIMEOUT", "500ms")

	const (
		token   = "send-secret-token"
		message = "unique-message-after-idle"
	)
	home, from := newHome(t)
	peerKey, peer, hook, wakes, body, auth := frozenPeer(t, token, func(up *atomic.Bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !up.Load() {
				drop(w)
				return
			}
			if r.URL.Path == "/" {
				writeInfo(w, peerKeyPlaceholder)
				return
			}
			if r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"messages":[]}`)
				return
			}
			w.WriteHeader(http.StatusCreated)
		}
	})
	if err := storeWake(t, home, peerKey, peer.URL, hook.URL+"/hooks/"+token, token); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runCLI(t, "send", peerKey, message)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if strings.TrimSpace(stdout) != "sent 1" {
		t.Fatalf("stdout %q stderr %q", stdout, stderr)
	}
	if wakes.Load() != 1 {
		t.Fatalf("wakes %d", wakes.Load())
	}
	assertWakeCall(t, body, auth, token, message, from)
	assertNoSecret(t, stderr, token, message)
	if strings.Contains(stdout, message) || strings.Contains(stdout, token) {
		t.Fatalf("stdout leaked %s", stdout)
	}
	if !strings.Contains(stderr, "peer wake: posted") || !strings.Contains(stderr, "is up") {
		t.Fatalf("stderr %s", stderr)
	}
}

func TestSendRateLimitsWake(t *testing.T) {
	t.Setenv("STRING_CAN_WAKE_WAIT", "150ms")
	t.Setenv("STRING_CAN_WAKE_POLL", "20ms")
	t.Setenv("STRING_CAN_WAKE_INTERVAL", "2m")
	t.Setenv("STRING_CAN_PEER_TIMEOUT", "300ms")

	const (
		token   = "rate-secret-token"
		message = "rate-limit-message-text"
	)
	home, _ := newHome(t)
	peerKey, peer, hook, wakes, _, _ := frozenPeer(t, token, func(up *atomic.Bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_ = up
			drop(w)
		}
	})
	if err := storeWake(t, home, peerKey, peer.URL, hook.URL, token); err != nil {
		t.Fatal(err)
	}
	_, stderr1, code := runCLI(t, "send", peerKey, message)
	if code != 0 {
		t.Fatalf("first exit %d %s", code, stderr1)
	}
	_, stderr2, code := runCLI(t, "send", peerKey, message+"-2")
	if code != 0 {
		t.Fatalf("second exit %d %s", code, stderr2)
	}
	if wakes.Load() != 1 {
		t.Fatalf("wakes %d", wakes.Load())
	}
	if !strings.Contains(stderr2, "skipped") {
		t.Fatalf("second stderr %s", stderr2)
	}
	assertNoSecret(t, stderr1+stderr2, token, message)
}

func TestFetchWakesFrozenPeer(t *testing.T) {
	t.Setenv("STRING_CAN_WAKE_WAIT", "2s")
	t.Setenv("STRING_CAN_WAKE_POLL", "10ms")
	t.Setenv("STRING_CAN_WAKE_INTERVAL", "2m")
	t.Setenv("STRING_CAN_PEER_TIMEOUT", "500ms")

	const (
		token   = "fetch-secret-token"
		message = "fetched-only-after-wake"
	)
	home, ourKey := newHome(t)
	_, peerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	peerKey := canon.PublicHexFromPrivate(peerPriv)
	body, err := canon.Encode(peerKey, 7, message)
	if err != nil {
		t.Fatal(err)
	}
	sig := canon.Sign(peerPriv, body)
	payload, err := json.Marshal(map[string]any{
		"messages": []map[string]any{{
			"id": 1, "from": peerKey, "ts": 7, "text": message, "signature": sig,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, peer, hook, wakes, wakeBody, auth := frozenPeer(t, token, func(up *atomic.Bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !up.Load() {
				drop(w)
				return
			}
			if r.URL.Path == "/" {
				writeInfo(w, peerKey)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(payload)
		}
	})
	if err := storeWake(t, home, peerKey, peer.URL, hook.URL+"/private/"+token, token); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runCLI(t, "fetch", peerKey)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `"text":"`+message+`"`) || !strings.Contains(stdout, "fetched 1") {
		t.Fatalf("stdout %s\nstderr %s", stdout, stderr)
	}
	if wakes.Load() != 1 {
		t.Fatalf("wakes %d stderr %s", wakes.Load(), stderr)
	}
	assertWakeCall(t, wakeBody, auth, token, message, ourKey)
	assertNoSecret(t, stderr, token, message)
	if strings.Contains(stderr, "private") {
		t.Fatalf("stderr leaked wake path %s", stderr)
	}
}

func TestHTTPFailureDoesNotWake(t *testing.T) {
	const token = "http-secret-token"
	home, _ := newHome(t)
	var wakes atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wakes.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	t.Cleanup(peer.Close)
	key := strings.Repeat("ef", 32)
	if err := storeWake(t, home, key, peer.URL, hook.URL, token); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runCLI(t, "send", key, "hello")
	if code != 0 {
		t.Fatalf("exit %d %s %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "pending") {
		t.Fatalf("stdout %s", stdout)
	}
	if wakes.Load() != 0 {
		t.Fatalf("woke an awake peer: %d stderr %s", wakes.Load(), stderr)
	}
	assertNoSecret(t, stdout+stderr, token, "")
}

func newHome(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("STRING_CAN_HOME", home)
	stdout, stderr, code := runCLI(t, "pubkey")
	if code != 0 {
		t.Fatalf("pubkey %d %s", code, stderr)
	}
	const prefix = "string-can public key: "
	line := strings.TrimSpace(stdout)
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("pubkey %q", stdout)
	}
	return home, strings.TrimPrefix(line, prefix)
}

func runCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.RunContext(context.Background(), args, &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func storeWake(t *testing.T, home, peerKey, baseURL, wakeURL, token string) error {
	t.Helper()
	st, err := store.Open(filepath.Join(home, "string-can.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.UpsertContact(store.Contact{PublicKey: peerKey, Label: "bot", BaseURL: baseURL}); err != nil {
		return err
	}
	return st.SetWake(peerKey, wakeURL, token)
}

const peerKeyPlaceholder = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

func frozenPeer(t *testing.T, token string, peerFn func(up *atomic.Bool) http.HandlerFunc) (peerKey string, peer, hook *httptest.Server, wakes *atomic.Int32, body *lockedBody, auth *lockedString) {
	t.Helper()
	var up atomic.Bool
	wakes = &atomic.Int32{}
	body = &lockedBody{}
	auth = &lockedString{}
	hook = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wakes.Add(1)
		auth.set(r.Header.Get("Authorization"))
		b, _ := io.ReadAll(r.Body)
		body.set(b)
		up.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)
	peer = httptest.NewServer(peerFn(&up))
	t.Cleanup(peer.Close)
	peerKey = peerKeyPlaceholder
	_ = token
	return peerKey, peer, hook, wakes, body, auth
}

type lockedBody struct {
	mu sync.Mutex
	b  []byte
}

func (l *lockedBody) set(b []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b = append([]byte(nil), b...)
}

func (l *lockedBody) get() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.b...)
}

type lockedString struct {
	mu sync.Mutex
	s  string
}

func (l *lockedString) set(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.s = s
}

func (l *lockedString) get() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.s
}

func drop(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	conn, _, err := hj.Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func writeInfo(w http.ResponseWriter, pub string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"v":1,"name":"bot","public_key":"`+pub+`"}`)
}

func assertWakeCall(t *testing.T, body *lockedBody, auth *lockedString, token, message, from string) {
	t.Helper()
	raw := string(body.get())
	if auth.get() != "Bearer "+token {
		t.Fatalf("auth %q", auth.get())
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("body %s: %v", raw, err)
	}
	if payload["type"] != "string-can.wake" || payload["from"] != from || len(payload) != 2 {
		t.Fatalf("body %s", raw)
	}
	if strings.Contains(raw, message) || strings.Contains(raw, token) {
		t.Fatalf("wake body leaked %s", raw)
	}
}

func assertNoSecret(t *testing.T, text, token, message string) {
	t.Helper()
	if token != "" && strings.Contains(text, token) {
		t.Fatalf("output contains token: %s", text)
	}
	if message != "" && strings.Contains(text, message) {
		t.Fatalf("output contains message: %s", text)
	}
}
