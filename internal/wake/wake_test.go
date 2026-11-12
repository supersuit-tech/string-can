package wake

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestMaskAndNormalizeProxy(t *testing.T) {
	const secret = "s3cret"
	raw := "https://proxyuser:" + secret + "@egress.example:8443/secret-path?" + secret + "=1"
	got := MaskProxy(raw)
	want := "https://********@egress.example:8443/..."
	if got != want {
		t.Fatalf("mask %q", got)
	}
	if strings.Contains(got, secret) || strings.Contains(got, "proxyuser") {
		t.Fatalf("mask leaked %q", got)
	}
	socks := MaskProxy("socks5://proxyuser:" + secret + "@10.1.1.1:1080")
	if socks != "socks5://********@10.1.1.1:1080" || strings.Contains(socks, secret) {
		t.Fatalf("socks %q", socks)
	}
	if MaskProxy("") != "" || MaskProxy("direct") != "direct" || MaskProxy("DIRECT") != "direct" {
		t.Fatal("direct/empty")
	}
	if MaskProxy("not a proxy") != "configured" {
		t.Fatal("non-url should not be echoed")
	}
	norm, err := NormalizeProxy("SOCKS5://proxyuser:" + secret + "@10.1.1.1:1080")
	if err != nil || !strings.Contains(norm, secret) || !strings.HasPrefix(norm, "socks5://") {
		t.Fatalf("normalize %q %v", norm, err)
	}
	cleared, err := NormalizeProxy("env")
	if err != nil || cleared != "" {
		t.Fatalf("env %q %v", cleared, err)
	}
	direct, err := NormalizeProxy("Direct")
	if err != nil || direct != "direct" {
		t.Fatalf("direct %q %v", direct, err)
	}
	_, err = NormalizeProxy("ftp://proxyuser:" + secret + "@egress.example")
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "proxyuser") {
		t.Fatalf("bad scheme error leaked: %v", err)
	}
}

func TestProxyLabelModes(t *testing.T) {
	resetProxyEnv(t)
	if ProxyLabel("") != ProxyEnv {
		t.Fatalf("unset %q", ProxyLabel(""))
	}
	t.Setenv("STRING_CAN_WAKE_PROXY", "direct")
	if ProxyLabel("") != ProxyDirect {
		t.Fatalf("env direct %q", ProxyLabel(""))
	}
	if ProxyLabel("direct") != ProxyDirect {
		t.Fatal("contact direct")
	}
	const secret = "s3cret"
	t.Setenv("STRING_CAN_WAKE_PROXY", "http://proxyuser:"+secret+"@egress.example:3128")
	if got := ProxyLabel(""); got != "http://********@egress.example:3128" || strings.Contains(got, secret) {
		t.Fatalf("env url %q", got)
	}
	// Per-contact wins over the env var.
	if ProxyLabel("direct") != ProxyDirect {
		t.Fatal("contact direct should hide the env proxy")
	}
	if got := ProxyLabel("http://other:" + secret + "@alt.example:9"); got != "http://********@alt.example:9" || strings.Contains(got, secret) {
		t.Fatalf("contact url %q", got)
	}
	t.Setenv("STRING_CAN_WAKE_PROXY", "ftp://proxyuser:"+secret+"@egress.example")
	if got := ProxyLabel(""); got != ProxyInvalid || strings.Contains(got, secret) {
		t.Fatalf("invalid %q", got)
	}
}

func TestRedactStripsProxyCredentials(t *testing.T) {
	const secret = "s3cret"
	proxy := "http://proxyuser:" + secret + "@egress.example:3128"
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("proxyuser:"+secret))
	msg := "Get \"" + proxy + "\": auth " + basic + " user proxyuser:" + secret
	got := redact(msg, "", "", proxy)
	if strings.Contains(got, secret) || strings.Contains(got, "proxyuser") || strings.Contains(got, basic) {
		t.Fatalf("redact leaked %q", got)
	}
	if !strings.Contains(got, "http://********@egress.example:3128") {
		t.Fatalf("redact %q", got)
	}
}

func TestWakeClientProxySelection(t *testing.T) {
	resetProxyEnv(t)
	t.Setenv("HTTP_PROXY", "http://env-proxy.example:8080")
	t.Setenv("HTTPS_PROXY", "http://env-proxy.example:8443")

	direct, err := wakeClient(ProxyDirect)
	if err != nil {
		t.Fatal(err)
	}
	if direct.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("direct client has a proxy")
	}

	fallback, err := wakeClient("")
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://wake-proxy-test.invalid/wake", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fallback.Transport.(*http.Transport).Proxy(req)
	if err != nil || got == nil || got.String() != "http://env-proxy.example:8080" {
		t.Fatalf("http fallback %v %v", got, err)
	}
	httpsReq, err := http.NewRequest(http.MethodPost, "https://wake-proxy-test.invalid/wake", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err = fallback.Transport.(*http.Transport).Proxy(httpsReq)
	if err != nil || got == nil || got.String() != "http://env-proxy.example:8443" {
		t.Fatalf("https fallback %v %v", got, err)
	}
	loop, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:9/wake", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err = fallback.Transport.(*http.Transport).Proxy(loop)
	if err != nil || got != nil {
		t.Fatalf("loopback should stay on the process proxy rules, got %v %v", got, err)
	}

	const secret = "s3cret"
	chosen, err := wakeClient("socks5://proxyuser:" + secret + "@socks.example:1080")
	if err != nil {
		t.Fatal(err)
	}
	got, err = chosen.Transport.(*http.Transport).Proxy(req)
	if err != nil || got == nil || got.Scheme != "socks5" || got.Host != "socks.example:1080" {
		t.Fatalf("socks5 %v %v", got, err)
	}
	pass, _ := got.User.Password()
	if pass != secret {
		t.Fatalf("socks password %q", pass)
	}
	// An explicit wake proxy is used even for loopback. The process proxy is not.
	got, err = chosen.Transport.(*http.Transport).Proxy(loop)
	if err != nil || got == nil || got.Host != "socks.example:1080" {
		t.Fatalf("explicit proxy for loopback %v %v", got, err)
	}
}

func TestTryWakeProxyURLIgnoresHTTPProxy(t *testing.T) {
	resetProxyEnv(t)
	const secret = "s3cret"
	good := startCountedProxy(t)
	bad := startCountedProxy(t)
	setProcessProxy(t, bad.URL)
	t.Setenv("STRING_CAN_WAKE_PROXY", withProxyUserinfo(good.URL, "proxyuser", secret))

	logs := tryWake(t, "http://wake-proxy-test.invalid/wake", "")
	if !strings.Contains(logs, "posted") {
		t.Fatalf("logs %s", logs)
	}
	if good.hits.Load() != 1 || bad.hits.Load() != 0 {
		t.Fatalf("good %d bad %d", good.hits.Load(), bad.hits.Load())
	}
	auth, host := good.seen()
	if host != "wake-proxy-test.invalid" {
		t.Fatalf("host %q", host)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("proxyuser:"+secret))
	if auth != want {
		t.Fatalf("auth %q", auth)
	}
	if strings.Contains(logs, secret) || strings.Contains(logs, "proxyuser") {
		t.Fatalf("logs leaked %s", logs)
	}
}

func TestTryWakeProxyDirectIgnoresHTTPProxy(t *testing.T) {
	resetProxyEnv(t)
	bad := startCountedProxy(t)
	setProcessProxy(t, bad.URL)
	t.Setenv("STRING_CAN_WAKE_PROXY", "direct")

	logs := tryWake(t, "http://wake-proxy-test.invalid/wake", "")
	if strings.Contains(logs, "posted") || bad.hits.Load() != 0 {
		t.Fatalf("direct used the process proxy hits=%d logs %s", bad.hits.Load(), logs)
	}
	if !strings.Contains(logs, "post failed") {
		t.Fatalf("logs %s", logs)
	}
}

func TestTryWakeProxyUnsetUsesProcessEnv(t *testing.T) {
	resetProxyEnv(t)
	good := startCountedProxy(t)
	setProcessProxy(t, good.URL)
	t.Setenv("STRING_CAN_WAKE_PROXY", "")

	logs := tryWake(t, "http://wake-proxy-test.invalid/wake", "")
	if !strings.Contains(logs, "posted") || good.hits.Load() != 1 {
		t.Fatalf("hits %d logs %s", good.hits.Load(), logs)
	}
	_, host := good.seen()
	if host != "wake-proxy-test.invalid" {
		t.Fatalf("host %q", host)
	}
}

func TestTryContactProxyOverridesEnv(t *testing.T) {
	resetProxyEnv(t)
	const secret = "s3cret"
	good := startCountedProxy(t)
	bad := startCountedProxy(t)
	setProcessProxy(t, bad.URL)
	t.Setenv("STRING_CAN_WAKE_PROXY", "direct")

	logs := tryWake(t, "http://wake-proxy-test.invalid/wake", withProxyUserinfo(good.URL, "proxyuser", secret))
	if !strings.Contains(logs, "posted") || good.hits.Load() != 1 || bad.hits.Load() != 0 {
		t.Fatalf("good %d bad %d logs %s", good.hits.Load(), bad.hits.Load(), logs)
	}
	if strings.Contains(logs, secret) || strings.Contains(logs, "proxyuser") {
		t.Fatalf("logs leaked %s", logs)
	}

	// The env URL would proxy even a loopback wake host. "direct" on the
	// contact has to win, or this POST never reaches the hook.
	var hooks atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hooks.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)
	t.Setenv("STRING_CAN_WAKE_PROXY", withProxyUserinfo(good.URL, "proxyuser", secret))
	before := good.hits.Load()
	logs = tryWake(t, hook.URL, ProxyDirect)
	if hooks.Load() != 1 || good.hits.Load() != before || !strings.Contains(logs, "posted") {
		t.Fatalf("hook %d proxy %d->%d logs %s", hooks.Load(), before, good.hits.Load(), logs)
	}
	if strings.Contains(logs, secret) || strings.Contains(logs, "proxyuser") {
		t.Fatalf("logs leaked %s", logs)
	}
}

func TestTryWakeProxyDoesNotAffectPeerPoll(t *testing.T) {
	resetProxyEnv(t)
	t.Setenv("STRING_CAN_WAKE_WAIT", "2s")
	t.Setenv("STRING_CAN_WAKE_POLL", "10ms")
	proxy := startCountedProxy(t)
	t.Setenv("STRING_CAN_WAKE_PROXY", proxy.URL)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"v":1,"name":"bot","public_key":"`+strings.Repeat("ab", 32)+`"}`)
	}))
	t.Cleanup(peer.Close)

	st := openWakeStore(t)
	peerKey := strings.Repeat("ab", 32)
	if err := st.UpsertContact(store.Contact{PublicKey: peerKey, Label: "bot", BaseURL: peer.URL}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWake(peerKey, "http://wake-proxy-test.invalid/wake", ""); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	if !Try(context.Background(), st, peerKey, peer.URL, strings.Repeat("cd", 32), func(format string, args ...any) {
		logs.WriteString(fmt.Sprintf(format, args...))
		logs.WriteByte('\n')
	}) {
		t.Fatalf("logs %s", logs.String())
	}
	if proxy.hits.Load() != 1 {
		t.Fatalf("wake proxy hits %d (peer poll must not use it) logs %s", proxy.hits.Load(), logs.String())
	}
	if !strings.Contains(logs.String(), "is up") {
		t.Fatalf("logs %s", logs.String())
	}
}

func TestInvalidWakeProxyIsNotLogged(t *testing.T) {
	resetProxyEnv(t)
	const secret = "s3cret"
	t.Setenv("STRING_CAN_WAKE_PROXY", "ftp://proxyuser:"+secret+"@egress.example")
	logs := tryWake(t, "http://wake-proxy-test.invalid/wake", "")
	if !strings.Contains(logs, "post failed") {
		t.Fatalf("logs %s", logs)
	}
	if strings.Contains(logs, secret) || strings.Contains(logs, "proxyuser") || strings.Contains(logs, "ftp://") {
		t.Fatalf("logs leaked %s", logs)
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

func resetProxyEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"HTTP_PROXY", "http_proxy",
		"HTTPS_PROXY", "https_proxy",
		"NO_PROXY", "no_proxy",
		"ALL_PROXY", "all_proxy",
		"REQUEST_METHOD",
		"STRING_CAN_WAKE_PROXY",
	} {
		t.Setenv(key, "")
	}
}

func setProcessProxy(t *testing.T, raw string) {
	t.Helper()
	t.Setenv("HTTP_PROXY", raw)
	t.Setenv("http_proxy", raw)
	t.Setenv("HTTPS_PROXY", raw)
	t.Setenv("https_proxy", raw)
}

func withProxyUserinfo(raw, user, pass string) string {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	u.User = url.UserPassword(user, pass)
	return u.String()
}

type countedProxy struct {
	URL  string
	hits atomic.Int32
	mu   sync.Mutex
	auth string
	host string
}

func startCountedProxy(t *testing.T) *countedProxy {
	t.Helper()
	p := &countedProxy{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hits.Add(1)
		host := r.Host
		if r.URL != nil && r.URL.Host != "" {
			host = r.URL.Host
		}
		p.mu.Lock()
		p.auth = r.Header.Get("Proxy-Authorization")
		p.host = host
		p.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	p.URL = srv.URL
	return p
}

func (p *countedProxy) seen() (auth, host string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.auth, p.host
}

func tryWake(t *testing.T, wakeURL, contactProxy string) string {
	t.Helper()
	st := openWakeStore(t)
	peerKey := strings.Repeat("ab", 32)
	if err := st.UpsertContact(store.Contact{PublicKey: peerKey, Label: "bot", BaseURL: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWake(peerKey, wakeURL, ""); err != nil {
		t.Fatal(err)
	}
	if contactProxy != "" {
		if err := st.SetWakeProxy(peerKey, contactProxy); err != nil {
			t.Fatal(err)
		}
	}
	var logs strings.Builder
	Try(context.Background(), st, peerKey, "", strings.Repeat("cd", 32), func(format string, args ...any) {
		logs.WriteString(fmt.Sprintf(format, args...))
		logs.WriteByte('\n')
	})
	return logs.String()
}
