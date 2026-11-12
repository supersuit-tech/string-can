package server_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supersuit-tech/string-can/internal/canon"
	"github.com/supersuit-tech/string-can/internal/cli"
	"github.com/supersuit-tech/string-can/internal/server"
	"github.com/supersuit-tech/string-can/internal/store"
)

type testServer struct {
	*server.Server
	cancel context.CancelFunc
	errCh  chan error
}

func (ts *testServer) Stop() {
	if ts == nil {
		return
	}
	if ts.cancel != nil {
		ts.cancel()
		ts.cancel = nil
	}
	if ts.errCh != nil {
		select {
		case <-ts.errCh:
		case <-time.After(5 * time.Second):
		}
		ts.errCh = nil
	}
	_ = ts.Close()
}

func TestTwoLocalServersSend(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	a := startServer(t, homeA, "alpha", 0, "", "")
	b := startServer(t, homeB, "beta", 0, "", "")

	run(t, homeA, 0, "connect", "--local", b.PublicKey(), b.BaseURL())
	run(t, homeB, 0, "connect", "--local", a.PublicKey(), a.BaseURL())
	out := run(t, homeA, 0, "send", b.PublicKey(), "hi")
	if !strings.HasPrefix(strings.TrimSpace(out), "sent ") || strings.Contains(out, "pending") {
		t.Fatalf("send: %q", out)
	}

	threadA := run(t, homeA, 0, "thread", b.PublicKey())
	threadB := run(t, homeB, 0, "thread", a.PublicKey())
	if !strings.Contains(threadA, `"text":"hi"`) {
		t.Fatalf("A thread missing hi: %s", threadA)
	}
	if !strings.Contains(threadB, `"text":"hi"`) {
		t.Fatalf("B thread missing hi: %s", threadB)
	}
	if !strings.Contains(threadA, `"from":"`+a.PublicKey()+`"`) {
		t.Fatalf("A thread from: %s", threadA)
	}
	if !strings.Contains(threadB, `"from":"`+a.PublicKey()+`"`) {
		t.Fatalf("B thread from: %s", threadB)
	}
}

func TestBadSignatureRejected(t *testing.T) {
	h := t.TempDir()
	srv := startServer(t, h, "host", 0, "", "")
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	from := canon.PublicHexFromPrivate(priv)
	mustContact(t, srv.Server, from, srv.BaseURL(), "peer")
	before := countMessages(t, h)

	body, err := canon.Encode(from, time.Now().UnixMilli(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.BaseURL()+"/"+from, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-String-Can-Signature", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if countMessages(t, h) != before {
		t.Fatal("row count changed")
	}
	got := getThread(t, srv.BaseURL(), from)
	if len(got) != 0 {
		t.Fatalf("GET returned %v", got)
	}
}

func TestUnknownContactRejected(t *testing.T) {
	h := t.TempDir()
	srv := startServer(t, h, "host", 0, "", "")
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	from := canon.PublicHexFromPrivate(priv)
	before := countMessages(t, h)
	body, err := canon.Encode(from, time.Now().UnixMilli(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	sig := canon.Sign(priv, body)
	status, _ := postMessage(t, srv.BaseURL(), from, sig, body)
	if status != http.StatusForbidden {
		t.Fatalf("status %d", status)
	}
	if countMessages(t, h) != before {
		t.Fatal("row count changed")
	}
	got := getThread(t, srv.BaseURL(), from)
	if len(got) != 0 {
		t.Fatalf("GET returned %v", got)
	}
}

func TestDuplicatePost(t *testing.T) {
	h := t.TempDir()
	srv := startServer(t, h, "host", 0, "", "")
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	from := canon.PublicHexFromPrivate(priv)
	mustContact(t, srv.Server, from, srv.BaseURL(), "peer")
	body, err := canon.Encode(from, time.Now().UnixMilli(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	sig := canon.Sign(priv, body)
	status, raw := postMessage(t, srv.BaseURL(), from, sig, body)
	if status != http.StatusCreated {
		t.Fatalf("first status %d %s", status, raw)
	}
	status, raw = postMessage(t, srv.BaseURL(), from, sig, body)
	if status != http.StatusOK {
		t.Fatalf("second status %d %s", status, raw)
	}
	var got struct {
		ID        int64 `json:"id"`
		Duplicate bool  `json:"duplicate"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Duplicate || got.ID == 0 {
		t.Fatalf("duplicate response: %s", raw)
	}
	if countMessages(t, h) != 1 {
		t.Fatalf("want 1 row, got %d", countMessages(t, h))
	}
}

func TestRenameLabelOnly(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	a := startServer(t, homeA, "alpha", 0, "", "")
	b := startServer(t, homeB, "beta", 0, "", "")
	run(t, homeA, 0, "connect", "--local", b.PublicKey(), b.BaseURL())
	out := run(t, homeA, 0, "rename", b.PublicKey(), "buddy")
	var c struct {
		Label     string `json:"label"`
		PublicKey string `json:"public_key"`
		BaseURL   string `json:"base_url"`
	}
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatal(err)
	}
	if c.Label != "buddy" || c.PublicKey != b.PublicKey() || c.BaseURL != b.BaseURL() {
		t.Fatalf("rename json: %s", out)
	}
	run(t, homeA, 0, "send", b.PublicKey(), "hi")
	got := getThread(t, a.BaseURL(), b.PublicKey())
	if len(got) != 1 || got[0].From != a.PublicKey() {
		t.Fatalf("GET path key changed: %+v", got)
	}
}

func TestNameWithoutRestart(t *testing.T) {
	h := t.TempDir()
	srv := startServer(t, h, "before", 0, "", "")
	info := getInfo(t, srv.BaseURL())
	if info.Name != "before" {
		t.Fatalf("name %q", info.Name)
	}
	run(t, h, 0, "name", "after")
	info = getInfo(t, srv.BaseURL())
	if info.Name != "after" {
		t.Fatalf("name after restart-less change: %q", info.Name)
	}
}

func TestWebhookNudge(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies [][]byte
		auths  []string
		failN  atomic.Int32
		hits   atomic.Int32
	)
	failN.Store(1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if failN.Add(-1) >= 0 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hook.Close)

	homeA := t.TempDir()
	homeB := t.TempDir()
	a := startServer(t, homeA, "alpha", 0, "", "")
	b := startServer(t, homeB, "beta", 0, hook.URL, "secret-token")
	run(t, homeA, 0, "connect", "--local", b.PublicKey(), b.BaseURL())
	run(t, homeB, 0, "connect", "--local", a.PublicKey(), a.BaseURL())
	run(t, homeA, 0, "send", b.PublicKey(), "SECRET_PAYLOAD")

	b.Tick(context.Background())
	if hits.Load() != 1 {
		t.Fatalf("hits after fail tick: %d", hits.Load())
	}
	b.Tick(context.Background())
	if hits.Load() != 2 {
		t.Fatalf("hits after success tick: %d", hits.Load())
	}
	b.Tick(context.Background())
	if hits.Load() != 2 {
		t.Fatalf("success retried: %d", hits.Load())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 2 {
		t.Fatal("missing webhook bodies")
	}
	for _, body := range bodies {
		s := string(body)
		if strings.Contains(s, "SECRET_PAYLOAD") {
			t.Fatalf("webhook contained message text: %s", s)
		}
		if !strings.Contains(s, `"mode":"now"`) {
			t.Fatalf("missing mode now: %s", s)
		}
		if !strings.Contains(s, a.PublicKey()) {
			t.Fatalf("missing from key: %s", s)
		}
	}
	if auths[0] != "Bearer secret-token" {
		t.Fatalf("auth %q", auths[0])
	}
}

func TestOutboundRetry(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	a := startServer(t, homeA, "alpha", 0, "", "")
	b := startServer(t, homeB, "beta", 0, "", "")
	bURL := b.BaseURL()
	bPort := mustPort(t, bURL)
	run(t, homeA, 0, "connect", "--local", b.PublicKey(), bURL)
	run(t, homeB, 0, "connect", "--local", a.PublicKey(), a.BaseURL())
	b.Stop()

	out := run(t, homeA, 0, "send", b.PublicKey(), "later")
	if !strings.Contains(out, "pending") {
		t.Fatalf("expected pending, got %q", out)
	}
	if strings.Contains(out, "unconfirmed") {
		t.Fatalf("connection refused is not unconfirmed: %q", out)
	}
	if countMessages(t, homeA) != 1 {
		t.Fatal("A should store the outbound row")
	}
	pending := undelivered(t, homeA)
	if len(pending) != 1 || pending[0].NextAttempt <= time.Now().UnixMilli() {
		t.Fatalf("expected a future retry, got %+v", pending)
	}

	startServer(t, homeB, "beta", bPort, "", "")
	a.Tick(context.Background())
	if msgs := undelivered(t, homeA); len(msgs) != 1 {
		t.Fatalf("retry ran before backoff: %+v", msgs)
	}
	if err := a.Store().RecordOutcome(pending[0].ID, pending[0].State, pending[0].LastError, pending[0].Attempts, 0); err != nil {
		t.Fatal(err)
	}
	a.Tick(context.Background())
	threadB := run(t, homeB, 0, "thread", a.PublicKey())
	if !strings.Contains(threadB, `"text":"later"`) {
		t.Fatalf("B missing delivered message: %s", threadB)
	}
	if msgs := undelivered(t, homeA); len(msgs) != 0 {
		t.Fatalf("still undelivered: %+v", msgs)
	}
}

func TestBodyTooLarge(t *testing.T) {
	h := t.TempDir()
	srv := startServer(t, h, "host", 0, "", "")
	from := strings.Repeat("ab", 32)
	mustContact(t, srv.Server, from, srv.BaseURL(), "peer")
	before := countMessages(t, h)
	body := bytes.Repeat([]byte("x"), canon.MaxBodyBytes+1)
	req, err := http.NewRequest(http.MethodPost, srv.BaseURL()+"/"+from, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d %s", resp.StatusCode, b)
	}
	if countMessages(t, h) != before {
		t.Fatal("row stored for oversized body")
	}
}

func startServer(t *testing.T, dir, name string, port int, webhook, token string) *testServer {
	t.Helper()
	srv, err := server.New(server.Config{
		Home:         dir,
		Name:         name,
		Port:         port,
		Local:        true,
		WakeEvery:    time.Hour,
		WebhookURL:   webhook,
		WebhookToken: token,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Bind(); err != nil {
		srv.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx) }()
	ts := &testServer{Server: srv, cancel: cancel, errCh: errCh}
	t.Cleanup(ts.Stop)
	waitURL(t, srv.BaseURL())
	return ts
}

func run(t *testing.T, homeDir string, want int, args ...string) string {
	t.Helper()
	t.Setenv("STRING_CAN_HOME", homeDir)
	var stdout, stderr bytes.Buffer
	code := cli.RunContext(context.Background(), args, &stdout, &stderr)
	if code != want {
		t.Fatalf("string-can %s: exit %d want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, want, stdout.String(), stderr.String())
	}
	return stdout.String()
}

func mustContact(t *testing.T, srv *server.Server, pub, base, label string) {
	t.Helper()
	if err := srv.Store().UpsertContact(store.Contact{
		PublicKey: pub,
		Label:     label,
		BaseURL:   base,
	}); err != nil {
		t.Fatal(err)
	}
}

func countMessages(t *testing.T, homeDir string) int64 {
	t.Helper()
	st, err := store.Open(filepath.Join(homeDir, "string-can.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	n, err := st.CountMessages()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func postMessage(t *testing.T, base, from, sig string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+"/"+from, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-String-Can-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

type threadMsg struct {
	ID        int64  `json:"id"`
	From      string `json:"from"`
	Ts        int64  `json:"ts"`
	Text      string `json:"text"`
	Signature string `json:"signature"`
}

func getThread(t *testing.T, base, peer string) []threadMsg {
	t.Helper()
	resp, err := http.Get(strings.TrimRight(base, "/") + "/" + peer)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET thread %d %s", resp.StatusCode, b)
	}
	var out struct {
		Messages []threadMsg `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Messages
}

func getInfo(t *testing.T, base string) struct {
	V         int    `json:"v"`
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
} {
	t.Helper()
	resp, err := http.Get(strings.TrimRight(base, "/") + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var info struct {
		V         int    `json:"v"`
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	return info
}

func waitURL(t *testing.T, u string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(strings.TrimRight(u, "/") + "/")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s did not become ready", u)
}

func undelivered(t *testing.T, homeDir string) []store.Message {
	t.Helper()
	st, err := store.Open(filepath.Join(homeDir, "string-can.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	msgs, err := st.Undelivered()
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func mustPort(t *testing.T, u string) int {
	t.Helper()
	addr := strings.TrimPrefix(u, "http://")
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
