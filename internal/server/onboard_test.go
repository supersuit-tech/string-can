package server_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supersuit-tech/string-can/internal/canon"
	"github.com/supersuit-tech/string-can/internal/cli"
	"github.com/supersuit-tech/string-can/internal/listenaddr"
	"github.com/supersuit-tech/string-can/internal/push"
	"github.com/supersuit-tech/string-can/internal/server"
	"github.com/supersuit-tech/string-can/internal/store"
)

func TestConnectRejectsLoopback(t *testing.T) {
	h := t.TempDir()
	t.Setenv("STRING_CAN_HOME", h)
	var stdout, stderr bytes.Buffer
	code := cli.RunContext(context.Background(), []string{"connect", "http://127.0.0.1:8741"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "refusing loopback URL") {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestConnectSavesWhenUnreachable(t *testing.T) {
	t.Cleanup(func() {
		cli.ProbeWait = 20 * time.Second
		cli.ProbeInterval = time.Second
	})
	cli.ProbeWait = 80 * time.Millisecond
	cli.ProbeInterval = 20 * time.Millisecond
	h := t.TempDir()
	key := strings.Repeat("ab", 32)
	t.Setenv("STRING_CAN_HOME", h)
	var stdout, stderr bytes.Buffer
	code := cli.RunContext(context.Background(), []string{"connect", "--local", key, "http://127.0.0.1:1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "saved; delivery will retry") {
		t.Fatalf("stderr %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), `"public_key":"`+key+`"`) {
		t.Fatalf("stdout %q", stdout.String())
	}
}

func TestConnectByURL(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	a := startServer(t, homeA, "alpha", 0, "", "")
	b := startServer(t, homeB, "beta", 0, "", "")
	out := run(t, homeA, 0, "connect", "--local", b.BaseURL())
	var c struct {
		Label     string `json:"label"`
		PublicKey string `json:"public_key"`
		BaseURL   string `json:"base_url"`
	}
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatal(err)
	}
	if c.PublicKey != b.PublicKey() || c.Label != "beta" || c.BaseURL != b.BaseURL() {
		t.Fatalf("connect json: %s", out)
	}
	_ = a
}

func TestHandshakeMutual(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	a := startServer(t, homeA, "alpha", 0, "", "")
	b := startServer(t, homeB, "beta", 0, "", "")
	run(t, homeA, 0, "connect", "--local", b.BaseURL())
	out := run(t, homeA, 0, "send", b.PublicKey(), "hello")
	if strings.Contains(out, "pending") {
		t.Fatalf("expected delivered send after handshake, got %q", out)
	}
	threadB := run(t, homeB, 0, "thread", a.PublicKey())
	if !strings.Contains(threadB, `"text":"hello"`) {
		t.Fatalf("B missing handshake-delivered message: %s", threadB)
	}
}

func TestInvite(t *testing.T) {
	h := t.TempDir()
	srv := startServer(t, h, "muse", 0, "", "")
	out := run(t, h, 0, "invite")
	want := "string-can connect " + srv.BaseURL() + "\n"
	if out != want {
		t.Fatalf("invite %q want %q", out, want)
	}
}

func TestConnectProbeRetry(t *testing.T) {
	t.Cleanup(func() {
		cli.ProbeWait = 20 * time.Second
		cli.ProbeInterval = time.Second
	})
	cli.ProbeWait = 3 * time.Second
	cli.ProbeInterval = 20 * time.Millisecond

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := canon.PublicHexFromPrivate(priv)
	var hits atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if r.URL.Path == "/" && r.Method == http.MethodGet {
			if n < 3 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"v": 1, "name": "late", "public_key": pub,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(hook.Close)

	h := t.TempDir()
	out := run(t, h, 0, "connect", "--local", hook.URL)
	var c struct {
		StatedName string `json:"stated_name"`
		PublicKey  string `json:"public_key"`
	}
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatal(err)
	}
	if c.StatedName != "late" || c.PublicKey != pub {
		t.Fatalf("retry connect: %s", out)
	}
	if hits.Load() < 3 {
		t.Fatalf("expected retries, hits=%d", hits.Load())
	}
}

func TestForbiddenIsActionable(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	a := startServer(t, homeA, "alpha", 0, "", "")
	b := startServer(t, homeB, "beta", 0, "", "")
	mustContact(t, a.Server, b.PublicKey(), b.BaseURL(), "beta")
	t.Setenv("STRING_CAN_HOME", homeA)
	var stdout, stderr bytes.Buffer
	code := cli.RunContext(context.Background(), []string{"send", b.PublicKey(), "hi"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("send exit %d stderr %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "pending") {
		t.Fatalf("stdout %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), push.ForbiddenContact) {
		t.Fatalf("stderr %q", stderr.String())
	}
	status := run(t, homeA, 0, "status")
	if !strings.Contains(status, `"last_error":"`) || !strings.Contains(status, "hasn") {
		t.Fatalf("status %s", status)
	}
	if !strings.Contains(status, `"state":"pending"`) {
		t.Fatalf("status state: %s", status)
	}
}

func TestFetchPullsPeerThread(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	a := startServer(t, homeA, "alpha", 0, "", "")
	b := startServer(t, homeB, "beta", 0, "", "")
	run(t, homeA, 0, "connect", "--local", b.BaseURL())
	run(t, homeB, 0, "connect", "--local", a.BaseURL())
	a.Stop()

	out := run(t, homeB, 0, "send", a.PublicKey(), "pulled")
	if !strings.Contains(out, "pending") {
		t.Fatalf("expected pending while A is down, got %q", out)
	}
	fetched := run(t, homeA, 0, "fetch", b.PublicKey())
	if !strings.Contains(fetched, `"text":"pulled"`) || !strings.Contains(fetched, "fetched 1") {
		t.Fatalf("fetch: %s", fetched)
	}
	threadA := run(t, homeA, 0, "thread", b.PublicKey())
	if !strings.Contains(threadA, `"text":"pulled"`) {
		t.Fatalf("A thread: %s", threadA)
	}
	again := run(t, homeA, 0, "fetch", b.PublicKey())
	if !strings.Contains(again, "fetched 0") {
		t.Fatalf("duplicate fetch: %s", again)
	}
}

func TestDoctorAndAllow(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	a := startServer(t, homeA, "alpha", 0, "", "")
	b := startServer(t, homeB, "beta", 0, "", "")
	run(t, homeA, 0, "connect", "--local", b.BaseURL())
	out := run(t, homeA, 0, "doctor")
	if !strings.Contains(out, "identity: ok") || !strings.Contains(out, "server: running") {
		t.Fatalf("doctor: %s", out)
	}
	if !strings.Contains(out, "bind: loopback") {
		t.Fatalf("doctor bind: %s", out)
	}
	if !strings.Contains(out, "reachable, mutual") {
		t.Fatalf("doctor contacts: %s", out)
	}
	if !strings.Contains(out, "webhook: not set") {
		t.Fatalf("doctor webhook: %s", out)
	}
	resp, err := http.Get(b.BaseURL() + "/allow/" + a.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var allow struct {
		Contact bool `json:"contact"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&allow); err != nil {
		t.Fatal(err)
	}
	if !allow.Contact {
		t.Fatal("expected handshake to add A on B")
	}
}

func TestAdvertiseHost(t *testing.T) {
	h := t.TempDir()
	srv, err := server.New(server.Config{
		Home:      h,
		Name:      "muse",
		Port:      0,
		Host:      "muse",
		Local:     true,
		WakeEvery: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if err := srv.Bind(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(srv.BaseURL(), "http://muse:") {
		t.Fatalf("advertised %s", srv.BaseURL())
	}
}

func TestBindRequiresLocalWithoutTailscale(t *testing.T) {
	old := listenaddr.FindTailscaleIPv4
	t.Cleanup(func() { listenaddr.FindTailscaleIPv4 = old })
	listenaddr.FindTailscaleIPv4 = func() (string, bool) { return "", false }

	h := t.TempDir()
	srv, err := server.New(server.Config{
		Home:          h,
		Name:          "x",
		Port:          0,
		TailscaleWait: 80 * time.Millisecond,
		WakeEvery:     time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	err = srv.BindContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--local") {
		t.Fatalf("err %v", err)
	}
}

func TestServeWebhookStatus(t *testing.T) {
	var auths []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hook.Close)

	h := t.TempDir()
	t.Setenv("STRING_CAN_HOME", h)
	t.Setenv("STRING_CAN_WEBHOOK_URL", hook.URL)
	t.Setenv("STRING_CAN_WEBHOOK_TOKEN", "secret-token")
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cli.RunContext(ctx, []string{"serve", "--local", "--name", "w"}, &stdout, &stderr)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(stdout.String(), "string-can listen:") && strings.Contains(stderr.String(), "self-test ok") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not exit")
	}
	if !strings.Contains(stdout.String(), "string-can public key:") {
		t.Fatalf("stdout %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "string-can webhook: configured, self-test ok") {
		t.Fatalf("stderr %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "bound to 127.0.0.1") {
		t.Fatalf("missing loopback warning: %s", stderr.String())
	}
	if len(auths) == 0 || auths[0] != "Bearer secret-token" {
		t.Fatalf("auth %v", auths)
	}
}

func TestWaitForTailscale(t *testing.T) {
	old := listenaddr.FindTailscaleIPv4
	t.Cleanup(func() { listenaddr.FindTailscaleIPv4 = old })
	var n atomic.Int32
	listenaddr.FindTailscaleIPv4 = func() (string, bool) {
		if n.Add(1) < 3 {
			return "", false
		}
		return "100.64.1.2", true
	}
	ip, err := listenaddr.WaitForTailscale(context.Background(), time.Second, 10*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "100.64.1.2" {
		t.Fatalf("ip %s", ip)
	}
}

func TestStoreLastError(t *testing.T) {
	h := t.TempDir()
	st, err := store.Open(filepath.Join(h, "string-can.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, _, err := st.InsertMessage(store.Message{
		PeerPub:   strings.Repeat("1", 64),
		FromPub:   strings.Repeat("2", 64),
		Ts:        1,
		Text:      "x",
		Body:      "{}",
		Signature: "sig",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetLastError(id, "boom"); err != nil {
		t.Fatal(err)
	}
	msgs, err := st.Undelivered()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].LastError != "boom" {
		t.Fatalf("%+v", msgs)
	}
	if err := st.MarkDelivered(id); err != nil {
		t.Fatal(err)
	}
	n, err := st.CountPending()
	if err != nil || n != 0 {
		t.Fatalf("pending %d %v", n, err)
	}
}
