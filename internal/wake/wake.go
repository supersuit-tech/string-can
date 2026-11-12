// Package wake thaws a frozen peer by POSTing its wake URL, then waiting
// until the peer answers. The POST carries no message text. The bearer
// token is never logged.
package wake

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/http/httpproxy"

	"github.com/supersuit-tech/string-can/internal/canon"
	"github.com/supersuit-tech/string-can/internal/push"
	"github.com/supersuit-tech/string-can/internal/store"
)

const (
	// DefaultWait is how long to poll a peer after a wake POST.
	DefaultWait = 60 * time.Second
	// DefaultInterval is the minimum gap between wake POSTs for one contact.
	DefaultInterval = 2 * time.Minute
	// DefaultPoll is the pause between health checks.
	DefaultPoll = time.Second
	// MaskedToken is the only token string safe to print.
	MaskedToken = "********"
	// ProxyEnv is shown when the wake POST uses the process proxy environment.
	ProxyEnv = "env"
	// ProxyDirect means the wake POST uses no proxy.
	ProxyDirect = "direct"
	// ProxyInvalid means the configured wake proxy cannot be used.
	ProxyInvalid = "invalid"
	wakeType     = "string-can.wake"
	proxyEnvKey  = "STRING_CAN_WAKE_PROXY"
)

// MaskURL returns scheme and host, with the path and query hidden.
// An empty URL stays empty. Anything that is not an http(s) URL is
// reported as "configured" so a raw secret is not echoed.
func MaskURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "configured"
	}
	masked := url.URL{Scheme: u.Scheme, Host: u.Host}
	if u.Path != "" && u.Path != "/" {
		masked.Path = "/..."
	} else {
		masked.Path = u.Path
	}
	return masked.String()
}

// NormalizeURL checks a wake URL and returns the canonical form.
// Userinfo is rejected so a secret is not stored in the URL.
func NormalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("wake URL must be http or https")
	}
	if u.User != nil {
		return "", fmt.Errorf("wake URL must not include a username or password; use --token-file or --token-env")
	}
	return u.String(), nil
}

// MaskProxy hides userinfo, query, and any non-root path in a proxy URL.
// "direct" stays "direct". An empty string stays empty. Anything that is
// not an http, https, or socks5 URL is reported as "configured".
func MaskProxy(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.EqualFold(raw, ProxyDirect) {
		return ProxyDirect
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "configured"
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5":
	default:
		return "configured"
	}
	// Build this by hand. url.URL.String escapes "*" in userinfo, which would
	// turn the mask into %2A and make the printed form hard to recognize.
	var b strings.Builder
	b.WriteString(strings.ToLower(u.Scheme))
	b.WriteString("://")
	if u.User != nil {
		b.WriteString(MaskedToken)
		b.WriteByte('@')
	}
	b.WriteString(u.Host)
	if u.Path != "" && u.Path != "/" {
		b.WriteString("/...")
	}
	return b.String()
}

// NormalizeProxy checks a wake proxy setting. "direct" means no proxy.
// "env" clears a per-contact override so the process environment applies.
// A URL must be http, https, or socks5. Userinfo is allowed and is returned
// unchanged; callers must not log it. The error text never includes raw.
func NormalizeProxy(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		return "", fmt.Errorf("wake proxy must be an http, https, or socks5 URL, or %q", ProxyDirect)
	case strings.EqualFold(raw, ProxyDirect):
		return ProxyDirect, nil
	case strings.EqualFold(raw, ProxyEnv):
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("wake proxy must be an http, https, or socks5 URL, or %q", ProxyDirect)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5":
		u.Scheme = strings.ToLower(u.Scheme)
	default:
		return "", fmt.Errorf("wake proxy must be an http, https, or socks5 URL, or %q", ProxyDirect)
	}
	return u.String(), nil
}

// EffectiveProxy resolves the proxy for one wake POST.
// A non-empty contactProxy wins over STRING_CAN_WAKE_PROXY.
// The result is "" (use HTTP_PROXY / HTTPS_PROXY / NO_PROXY), "direct",
// or a proxy URL. The error does not include the setting.
func EffectiveProxy(contactProxy string) (string, error) {
	raw := strings.TrimSpace(contactProxy)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv(proxyEnvKey))
	}
	if raw == "" {
		return "", nil
	}
	return NormalizeProxy(raw)
}

// ProxyLabel is the proxy mode a wake POST will use: "env", "direct",
// "invalid", or a proxy URL with userinfo masked.
func ProxyLabel(contactProxy string) string {
	spec, err := EffectiveProxy(contactProxy)
	if err != nil {
		return ProxyInvalid
	}
	switch spec {
	case "":
		return ProxyEnv
	case ProxyDirect:
		return ProxyDirect
	default:
		return MaskProxy(spec)
	}
}

// LoadToken reads a bearer token from a file, an environment variable, or
// a literal. At most one source may be set. An empty set of sources returns
// an empty token. The warning is set when literal is used, because that
// value is stored in shell history.
func LoadToken(file, envName, literal string) (token, warning string, err error) {
	n := 0
	if file != "" {
		n++
	}
	if envName != "" {
		n++
	}
	if literal != "" {
		n++
	}
	if n > 1 {
		return "", "", fmt.Errorf("pass only one wake token source (file, env, or literal)")
	}
	switch {
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", "", fmt.Errorf("read wake token file: %w", err)
		}
		if len(b) > 4096 {
			return "", "", fmt.Errorf("wake token file is too large")
		}
		tok := strings.TrimSpace(string(b))
		if tok == "" {
			return "", "", fmt.Errorf("wake token file is empty")
		}
		if strings.ContainsAny(tok, "\r\n") {
			return "", "", fmt.Errorf("wake token file must be one line")
		}
		return tok, "", nil
	case envName != "":
		if !validEnvName(envName) {
			return "", "", fmt.Errorf("invalid environment variable name %q", envName)
		}
		tok := strings.TrimSpace(os.Getenv(envName))
		if tok == "" {
			return "", "", fmt.Errorf("environment variable %s is empty", envName)
		}
		return tok, "", nil
	case literal != "":
		return literal, "warning: passing the wake token on the command line stores it in shell history; prefer --token-file or --token-env", nil
	default:
		return "", "", nil
	}
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if r == '_' || unicode.IsLetter(r) {
			continue
		}
		if i > 0 && unicode.IsDigit(r) {
			continue
		}
		return false
	}
	return true
}

// Settings reads STRING_CAN_WAKE_WAIT, STRING_CAN_WAKE_INTERVAL, and
// STRING_CAN_WAKE_POLL. Invalid or non-positive values use the defaults.
func Settings() (wait, interval, poll time.Duration) {
	return durationEnv("STRING_CAN_WAKE_WAIT", DefaultWait),
		durationEnv("STRING_CAN_WAKE_INTERVAL", DefaultInterval),
		durationEnv("STRING_CAN_WAKE_POLL", DefaultPoll)
}

func durationEnv(key string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// SendNeedsWake reports whether a failed or unconfirmed delivery should
// thaw the peer. An HTTP status means the peer already answered, so it is
// not frozen.
func SendNeedsWake(kind push.Kind, err error) bool {
	switch kind {
	case push.KindUnconfirmed:
		return true
	case push.KindPending:
		var he *push.HTTPError
		if errors.As(err, &he) {
			return false
		}
		return true
	default:
		return false
	}
}

// FetchNeedsWake reports whether a fetch error means the peer did not answer.
// "peer returned N" means the peer is up.
func FetchNeedsWake(err error) bool {
	if err == nil {
		return false
	}
	if strings.Contains(err.Error(), "peer returned ") {
		return false
	}
	return true
}

// Try posts the contact's wake URL when the rate limit allows, polls the
// peer's health endpoint, and reports whether the caller should retry.
// It never includes message text. Failures are logged and returned as
// false so the caller's existing retry path continues.
func Try(ctx context.Context, st *store.Store, peerPub, peerURL, from string, logf func(string, ...any)) bool {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if st == nil || ctx.Err() != nil {
		return false
	}
	defer func() {
		if rec := recover(); rec != nil {
			logf("peer wake: internal error")
		}
	}()
	wakeURL, token, proxy, err := st.LoadWake(peerPub)
	if err != nil {
		logf("peer wake: %s", redact(err.Error(), "", ""))
		return false
	}
	if wakeURL == "" {
		return false
	}
	wait, interval, poll := Settings()
	now := time.Now()
	claimed, err := st.ClaimWake(peerPub, now.UnixMilli(), interval.Milliseconds())
	if err != nil {
		logf("peer wake: %s not claimed: %s", canon.ShortKey(peerPub), redact(err.Error(), wakeURL, token, proxy))
		return false
	}
	if !claimed {
		logf("peer wake: skipped %s (at most once per %s)", canon.ShortKey(peerPub), interval)
		return false
	}
	if err := post(ctx, wakeURL, token, from, proxy); err != nil {
		logf("peer wake: post failed for %s: %s", canon.ShortKey(peerPub), redact(err.Error(), wakeURL, token, proxy, os.Getenv(proxyEnvKey)))
		return false
	}
	logf("peer wake: posted %s for %s", MaskURL(wakeURL), canon.ShortKey(peerPub))
	if ctx.Err() != nil {
		return false
	}
	if peerURL == "" {
		return true
	}
	if pollPeer(ctx, peerURL, wait, poll) {
		logf("peer wake: %s is up", canon.ShortKey(peerPub))
	} else if ctx.Err() != nil {
		return false
	} else {
		logf("peer wake: %s still down after %s", canon.ShortKey(peerPub), wait)
	}
	return true
}

func post(ctx context.Context, wakeURL, token, from, contactProxy string) error {
	body, err := encode(from)
	if err != nil {
		return err
	}
	spec, err := EffectiveProxy(contactProxy)
	if err != nil {
		return err
	}
	client, err := wakeClient(spec)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wakeURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New(redact(err.Error(), wakeURL, token, contactProxy, spec))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("wake webhook returned %d", resp.StatusCode)
	}
	return nil
}

func encode(from string) ([]byte, error) {
	payload := struct {
		Type string `json:"type"`
		From string `json:"from"`
	}{Type: wakeType, From: from}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// pollPeer uses the process proxy, the same transport as send and fetch.
// Only the wake POST goes through wakeClient.
func pollPeer(ctx context.Context, peerURL string, wait, every time.Duration) bool {
	if wait <= 0 {
		return false
	}
	if every <= 0 {
		every = DefaultPoll
	}
	deadline := time.Now().Add(wait)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		if ctx.Err() != nil {
			return false
		}
		reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := push.GetInfo(reqCtx, client, peerURL)
		cancel()
		if err == nil {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		sleep := every
		if remain := time.Until(deadline); sleep > remain {
			sleep = remain
		}
		if sleep <= 0 {
			return false
		}
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// wakeClient is the HTTP client for the wake POST only.
// spec "" uses HTTP_PROXY, HTTPS_PROXY, and NO_PROXY, read at request time.
// spec "direct" uses no proxy. Any other spec is a proxy URL.
func wakeClient(spec string) (*http.Client, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok || base == nil {
		base = &http.Transport{}
	}
	tr := base.Clone()
	switch spec {
	case "":
		tr.Proxy = processProxy
	case ProxyDirect:
		tr.Proxy = nil
	default:
		u, err := url.Parse(spec)
		if err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("wake proxy must be an http, https, or socks5 URL, or %q", ProxyDirect)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: tr}, nil
}

// processProxy matches net/http's ProxyFromEnvironment. The standard helper
// caches the first environment lookup for the life of the process; reading
// it here keeps the unset fallback equal to the process proxy env at send time.
func processProxy(req *http.Request) (*url.URL, error) {
	if req == nil || req.URL == nil {
		return nil, nil
	}
	return httpproxy.FromEnvironment().ProxyFunc()(req.URL)
}

func redact(msg, wakeURL, token string, proxies ...string) string {
	// Replace proxy URLs before the wake URL. A proxy userinfo string can
	// contain the same secret as the webhook path; replacing the token
	// first would split the URL and leave the rest in the log.
	for _, proxy := range proxies {
		msg = redactProxy(msg, proxy)
	}
	if wakeURL != "" {
		msg = strings.ReplaceAll(msg, wakeURL, MaskURL(wakeURL))
	}
	if token != "" {
		msg = strings.ReplaceAll(msg, "Bearer "+token, "Bearer "+MaskedToken)
		msg = strings.ReplaceAll(msg, token, MaskedToken)
	}
	return msg
}

func redactProxy(msg, proxy string) string {
	proxy = strings.TrimSpace(proxy)
	if proxy == "" || strings.EqualFold(proxy, ProxyDirect) || strings.EqualFold(proxy, ProxyEnv) {
		return msg
	}
	if masked := MaskProxy(proxy); masked != "" && masked != "configured" {
		msg = strings.ReplaceAll(msg, proxy, masked)
	} else {
		msg = strings.ReplaceAll(msg, proxy, "configured")
	}
	u, err := url.Parse(proxy)
	if err != nil || u == nil || u.User == nil {
		return msg
	}
	user := u.User.Username()
	pass, _ := u.User.Password()
	if info := u.User.String(); info != "" {
		msg = strings.ReplaceAll(msg, info, MaskedToken)
	}
	if pass != "" {
		msg = strings.ReplaceAll(msg, pass, MaskedToken)
		if enc := url.PathEscape(pass); enc != pass {
			msg = strings.ReplaceAll(msg, enc, MaskedToken)
		}
		if enc := url.QueryEscape(pass); enc != pass {
			msg = strings.ReplaceAll(msg, enc, MaskedToken)
		}
	}
	if len(user) >= 3 {
		msg = strings.ReplaceAll(msg, user, MaskedToken)
	}
	raw := user + ":" + pass
	msg = strings.ReplaceAll(msg, "Basic "+base64.StdEncoding.EncodeToString([]byte(raw)), "Basic "+MaskedToken)
	return msg
}
