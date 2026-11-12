package server_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestServeWaitTimeout(t *testing.T) {
	old := listenaddr.FindTailscaleIPv4
	t.Cleanup(func() { listenaddr.FindTailscaleIPv4 = old })
	listenaddr.FindTailscaleIPv4 = func() (string, bool) { return "", false }

	h := t.TempDir()
	t.Setenv("STRING_CAN_HOME", h)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := cli.RunContext(ctx, []string{"serve", "--name", "x", "--wait", "80ms"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--outbound-only") && !strings.Contains(stderr.String(), "--local") {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestServeOutboundOnly(t *testing.T) {
	old := listenaddr.FindTailscaleIPv4
	t.Cleanup(func() { listenaddr.FindTailscaleIPv4 = old })
	listenaddr.FindTailscaleIPv4 = func() (string, bool) { return "", false }

	h := t.TempDir()
	t.Setenv("STRING_CAN_HOME", h)
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cli.RunContext(ctx, []string{"serve", "--outbound-only", "--name", "muse"}, &stdout, &stderr)
	}()
	deadline := time.Now().Add(5 * time.Second)
	listen := ""
	for time.Now().Before(deadline) {
		out := stdout.String()
		if strings.Contains(out, "string-can listen:") && strings.Contains(stderr.String(), "outbound-only") {
			for _, line := range strings.Split(out, "\n") {
				if rest, ok := strings.CutPrefix(line, "string-can listen: "); ok {
					listen = rest
				}
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if listen == "" {
		cancel()
		t.Fatalf("missing listen line: stdout %s stderr %s", stdout.String(), stderr.String())
	}
	info := getInfo(t, listen)
	if !info.OutboundOnly {
		cancel()
		t.Fatalf("GET / should advertise outbound_only: %+v", info)
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
	if !strings.Contains(stderr.String(), "fetch --watch") {
		t.Fatalf("stderr %s", stderr.String())
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

func TestOutboundOnlyHandshakeAndSend(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	a := startServer(t, homeA, "alpha", 0, "", "")
	b := startOutbound(t, homeB, "muse", 0)
	run(t, homeB, 0, "connect", "--local", a.BaseURL())

	out := run(t, homeA, 0, "contacts")
	if !strings.Contains(out, `"pull_only":true`) {
		t.Fatalf("A should mark muse pull-only: %s", out)
	}
	sent := run(t, homeA, 0, "send", b.PublicKey(), "for-muse")
	if !strings.Contains(sent, "(pull)") || strings.Contains(sent, "pending") {
		t.Fatalf("send: %q", sent)
	}
	status := run(t, homeA, 0, "status")
	if strings.TrimSpace(status) != "" {
		t.Fatalf("status should be empty, got %q", status)
	}
	fetched := run(t, homeB, 0, "fetch", a.PublicKey())
	if !strings.Contains(fetched, `"text":"for-muse"`) || !strings.Contains(fetched, "fetched 1") {
		t.Fatalf("fetch: %s", fetched)
	}
	doc := run(t, homeA, 0, "doctor")
	if !strings.Contains(doc, "pull-only") {
		t.Fatalf("doctor: %s", doc)
	}
}

func TestConnectPullOnlyKeyOnly(t *testing.T) {
	h := t.TempDir()
	startServer(t, h, "alpha", 0, "", "")
	key := strings.Repeat("ab", 32)
	out := run(t, h, 0, "connect", "--pull-only", key)
	var c struct {
		PublicKey string `json:"public_key"`
		PullOnly  bool   `json:"pull_only"`
		BaseURL   string `json:"base_url"`
	}
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatal(err)
	}
	if !c.PullOnly || c.PublicKey != key || c.BaseURL != "" {
		t.Fatalf("connect json: %s", out)
	}
	doc := run(t, h, 0, "doctor")
	if !strings.Contains(doc, "pull-only (expected)") {
		t.Fatalf("doctor: %s", doc)
	}
}

func TestInviteOutboundOnly(t *testing.T) {
	h := t.TempDir()
	srv := startOutbound(t, h, "muse", 0)
	t.Setenv("STRING_CAN_HOME", h)
	var stdout, stderr bytes.Buffer
	code := cli.RunContext(context.Background(), []string{"invite"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d %s %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "string-can connect --pull-only "+srv.PublicKey()) {
		t.Fatalf("invite stdout %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "outbound-only") {
		t.Fatalf("invite stderr %q", stderr.String())
	}
}

func TestHandshakeOutboundOnlyLoopbackFromRemote(t *testing.T) {
	h := t.TempDir()
	srv := startServer(t, h, "host", 0, "", "")
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	from := canon.PublicHexFromPrivate(priv)
	body, err := push.EncodeHandshake(from, "muse", "http://127.0.0.1:8741", true)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.BaseURL()+"/connect", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.RemoteAddr = "100.64.1.2:4242"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-String-Can-Signature", canon.Sign(priv, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d %s", resp.StatusCode, b)
	}
	c, err := srv.Store().GetContact(from)
	if err != nil || !c.PullOnly {
		t.Fatalf("contact %+v %v", c, err)
	}
}

func TestDoctorTailnetOffline(t *testing.T) {
	oldFind := listenaddr.FindTailscaleIPv4
	oldStatus := listenaddr.ReadTailscaleStatus
	t.Cleanup(func() {
		listenaddr.FindTailscaleIPv4 = oldFind
		listenaddr.ReadTailscaleStatus = oldStatus
	})
	listenaddr.FindTailscaleIPv4 = func() (string, bool) { return "100.64.1.2", true }
	listenaddr.ReadTailscaleStatus = func(context.Context) ([]byte, error) {
		return []byte(`{"Self":{"Online":false}}`), nil
	}

	h := t.TempDir()
	startServer(t, h, "alpha", 0, "", "")
	if err := os.WriteFile(filepath.Join(h, "listen.url"), []byte("http://100.64.1.2:8741\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STRING_CAN_HOME", h)
	var stdout, stderr bytes.Buffer
	code := cli.RunContext(context.Background(), []string{"doctor"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Self.Online=false") {
		t.Fatalf("doctor: %s", stdout.String())
	}
}

func TestFetchWatch(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	a := startServer(t, homeA, "alpha", 0, "", "")
	b := startServer(t, homeB, "beta", 0, "", "")
	run(t, homeA, 0, "connect", "--local", b.BaseURL())
	run(t, homeB, 0, "connect", "--local", a.BaseURL())
	a.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	t.Setenv("STRING_CAN_HOME", homeA)
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cli.RunContext(ctx, []string{"fetch", "--watch", "--every", "30ms", b.PublicKey()}, &stdout, &stderr)
	}()
	time.Sleep(60 * time.Millisecond)
	out := run(t, homeB, 0, "send", a.PublicKey(), "watched")
	if !strings.Contains(out, "pending") {
		t.Fatalf("expected pending send while A is down, got %q", out)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(stdout.String(), `"text":"watched"`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fetch --watch did not exit")
	}
	if !strings.Contains(stdout.String(), `"text":"watched"`) {
		t.Fatalf("watch stdout %s stderr %s", stdout.String(), stderr.String())
	}
}

func startOutbound(t *testing.T, dir, name string, port int) *testServer {
	t.Helper()
	srv, err := server.New(server.Config{
		Home:         dir,
		Name:         name,
		Port:         port,
		OutboundOnly: true,
		WakeEvery:    time.Hour,
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
