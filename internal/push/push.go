package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ForbiddenContact is the actionable 403 explanation.
const ForbiddenContact = "peer rejected the message: it hasn't added you as a contact yet; ask them to run connect with your URL"

// DefaultClient is used for peer POSTs and webhooks.
func DefaultClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

// HTTPError is a non-success status from a peer or webhook.
type HTTPError struct {
	Status int
	Msg    string
}

func (e *HTTPError) Error() string {
	if e == nil {
		return ""
	}
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("peer returned %d", e.Status)
}

// Message POSTs a signed canonical body to the peer. 200 and 201 count as success.
func Message(ctx context.Context, client *http.Client, baseURL, fromPub, signature string, body []byte) error {
	if client == nil {
		client = DefaultClient()
	}
	u := strings.TrimRight(baseURL, "/") + "/" + fromPub
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-String-Can-Signature", signature)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return nil
	}
	if resp.StatusCode == http.StatusForbidden {
		return &HTTPError{Status: resp.StatusCode, Msg: ForbiddenContact}
	}
	msg := fmt.Sprintf("peer returned %d", resp.StatusCode)
	if len(raw) > 0 {
		msg = fmt.Sprintf("%s: %s", msg, strings.TrimSpace(string(raw)))
	}
	return &HTTPError{Status: resp.StatusCode, Msg: msg}
}

// IsForbidden reports whether err is a 403 from the peer.
func IsForbidden(err error) bool {
	var he *HTTPError
	return asHTTPError(err, &he) && he.Status == http.StatusForbidden
}

func asHTTPError(err error, target **HTTPError) bool {
	if err == nil {
		return false
	}
	he, ok := err.(*HTTPError)
	if !ok {
		return false
	}
	*target = he
	return true
}

// RemoteMessage is one entry from a peer's GET thread.
type RemoteMessage struct {
	ID        int64  `json:"id"`
	From      string `json:"from"`
	Ts        int64  `json:"ts"`
	Text      string `json:"text"`
	Signature string `json:"signature"`
}

// Thread GETs the peer's copy of the conversation with us.
func Thread(ctx context.Context, client *http.Client, baseURL, ourPub string) ([]RemoteMessage, error) {
	if client == nil {
		client = DefaultClient()
	}
	u := strings.TrimRight(baseURL, "/") + "/" + ourPub
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("peer returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Messages []RemoteMessage `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Messages, nil
}

// HandshakeBody is the signed JSON posted to POST /connect.
type HandshakeBody struct {
	V            int    `json:"v"`
	PublicKey    string `json:"public_key"`
	Name         string `json:"name"`
	BaseURL      string `json:"base_url"`
	OutboundOnly bool   `json:"outbound_only,omitempty"`
}

// EncodeHandshake returns compact JSON for a connect handshake.
func EncodeHandshake(pub, name, baseURL string, outboundOnly bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(HandshakeBody{V: 1, PublicKey: pub, Name: name, BaseURL: baseURL, OutboundOnly: outboundOnly}); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// Handshake POSTs a signed identity to the peer's /connect so they add us.
func Handshake(ctx context.Context, client *http.Client, peerURL, signature string, body []byte) error {
	if client == nil {
		client = DefaultClient()
	}
	u := strings.TrimRight(peerURL, "/") + "/connect"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-String-Can-Signature", signature)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return nil
	}
	return fmt.Errorf("peer returned %d", resp.StatusCode)
}

// Allow GETs whether the peer has us as a contact.
func Allow(ctx context.Context, client *http.Client, peerURL, ourPub string) (allowed bool, ok bool, err error) {
	if client == nil {
		client = DefaultClient()
	}
	u := strings.TrimRight(peerURL, "/") + "/allow/" + ourPub
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, false, fmt.Errorf("peer returned %d", resp.StatusCode)
	}
	var out struct {
		Contact bool `json:"contact"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, false, err
	}
	return out.Contact, true, nil
}

// Info is GET / on a peer.
type Info struct {
	V            int    `json:"v"`
	Name         string `json:"name"`
	PublicKey    string `json:"public_key"`
	OutboundOnly bool   `json:"outbound_only"`
}

// GetInfo GETs the peer's identity document.
func GetInfo(ctx context.Context, client *http.Client, baseURL string) (Info, error) {
	if client == nil {
		client = DefaultClient()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/", nil)
	if err != nil {
		return Info{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Info{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Info{}, fmt.Errorf("peer returned %d", resp.StatusCode)
	}
	var info Info
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return Info{}, err
	}
	return info, nil
}

// NudgePayload is the webhook body. It names the keys and does not include message text.
func NudgePayload(fromPub, peerPub string) ([]byte, error) {
	payload := struct {
		Text string `json:"text"`
		Mode string `json:"mode"`
	}{
		Text: fmt.Sprintf("new message from %s in thread %s", fromPub, peerPub),
		Mode: "now",
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// SelfTestPayload is the webhook self-test body.
func SelfTestPayload() ([]byte, error) {
	payload := struct {
		Text string `json:"text"`
		Mode string `json:"mode"`
	}{
		Text: "string-can webhook self-test",
		Mode: "now",
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// Nudge POSTs one wake-up to webhookURL. token, when set, is sent as a Bearer token.
func Nudge(ctx context.Context, client *http.Client, webhookURL, token, fromPub, peerPub string) error {
	body, err := NudgePayload(fromPub, peerPub)
	if err != nil {
		return err
	}
	return postWebhook(ctx, client, webhookURL, token, body)
}

// SelfTest POSTs a webhook self-test. It does not include a message body.
func SelfTest(ctx context.Context, client *http.Client, webhookURL, token string) error {
	body, err := SelfTestPayload()
	if err != nil {
		return err
	}
	return postWebhook(ctx, client, webhookURL, token, body)
}

func postWebhook(ctx context.Context, client *http.Client, webhookURL, token string, body []byte) error {
	if client == nil {
		client = DefaultClient()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}
