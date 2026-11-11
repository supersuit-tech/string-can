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

// DefaultClient is used for peer POSTs and webhooks.
func DefaultClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
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
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return nil
	}
	return fmt.Errorf("peer returned %d", resp.StatusCode)
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

// Nudge POSTs one wake-up to webhookURL. token, when set, is sent as a Bearer token.
func Nudge(ctx context.Context, client *http.Client, webhookURL, token, fromPub, peerPub string) error {
	if client == nil {
		client = DefaultClient()
	}
	body, err := NudgePayload(fromPub, peerPub)
	if err != nil {
		return err
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
