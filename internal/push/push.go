package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/supersuit-tech/string-can/internal/canon"
)

// ForbiddenContact is the actionable 403 explanation.
const ForbiddenContact = "peer rejected the message: it hasn't added you as a contact yet; ask them to run connect with your URL"

// UnconfirmedDetail is the user-facing note when a response was lost.
// The peer may already have stored the message.
const UnconfirmedDetail = "response timed out, will verify"

// AbsentDetail is the user-facing note when a lost response was checked
// and the peer's thread does not contain the message.
const AbsentDetail = "response timed out and the peer does not have the message"

const defaultPeerTimeout = 20 * time.Second

// DefaultClient is used for probes and webhooks.
func DefaultClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

// PeerClient is used for message delivery and the thread check that follows
// a lost response. STRING_CAN_PEER_TIMEOUT overrides the default (20s),
// as a Go duration such as "30s".
func PeerClient() *http.Client {
	return &http.Client{Timeout: peerTimeout()}
}

func peerTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("STRING_CAN_PEER_TIMEOUT"))
	if raw == "" {
		return defaultPeerTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return defaultPeerTimeout
	}
	return d
}

// RetryDelay is the wait after the n-th failed delivery attempt (1-based):
// 5s, 15s, 30s, then 60s.
func RetryDelay(failures int) time.Duration {
	steps := [...]time.Duration{
		5 * time.Second,
		15 * time.Second,
		30 * time.Second,
		60 * time.Second,
	}
	if failures <= 1 {
		return steps[0]
	}
	i := failures - 1
	if i >= len(steps) {
		i = len(steps) - 1
	}
	return steps[i]
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
		client = PeerClient()
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

// Kind is how a delivery attempt ended.
type Kind int

const (
	// KindDelivered means the peer accepted the message or already has it.
	KindDelivered Kind = iota + 1
	// KindPending means the peer did not accept the message.
	KindPending
	// KindUnconfirmed means the request may have arrived and the response was lost.
	KindUnconfirmed
)

// Outcome is the result of Deliver.
type Outcome struct {
	Kind Kind
	Err  error
	// Detail is safe to show to a user. Timeouts are rewritten; a refused
	// connection or an HTTP error is quoted as the peer reported it.
	Detail string
	// Found is true when the peer's thread contained the signature.
	Found bool
	// Absent is true when the thread was read and the signature was not there.
	Absent bool
	// FilesSkipped is true when the text was delivered and the files were not.
	// An older peer, or a peer that rejected the bytes, still has the text.
	FilesSkipped bool
}

// ErrBlobsUnsupported means the peer has no file endpoint. The text message
// can still stand on its own. Older peers answer the blob URL with 404.
var ErrBlobsUnsupported = errors.New("peer does not accept files")

// BlobReject is a file upload the peer refused. Retrying will not succeed.
type BlobReject struct {
	Status int
	Msg    string
}

func (e *BlobReject) Error() string {
	if e == nil {
		return "peer rejected file"
	}
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("peer rejected file (%d)", e.Status)
}

// Permanent reports whether another upload of the same bytes would fail too.
func (e *BlobReject) Permanent() bool {
	if e == nil {
		return false
	}
	switch e.Status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestEntityTooLarge:
		return true
	default:
		return false
	}
}

// DeliverOptions controls Deliver.
type DeliverOptions struct {
	// VerifyFirst reads the peer's thread before POSTing. Used when an
	// earlier attempt timed out, so a retry does not POST a message the
	// peer already stored.
	VerifyFirst bool
	// OpenBlob opens a local blob by SHA-256. When the message has files,
	// Deliver uploads each one after the peer accepts the message. Nil
	// OpenBlob with a file message is a failed delivery: the bytes never left.
	OpenBlob func(sha string) (io.ReadCloser, error)
}

// Deliver POSTs a signed message. A timeout or a dropped response is not
// treated as failure until the peer's thread has been checked for the
// signature. Connection refused, DNS failure, no route, and an HTTP error
// status are pending: the peer did not accept this attempt.
func Deliver(ctx context.Context, client *http.Client, baseURL, fromPub, signature string, body []byte, opt DeliverOptions) Outcome {
	if client == nil {
		client = PeerClient()
	}
	absent := false
	if opt.VerifyFirst && ctx.Err() == nil {
		found, err := signatureOnThread(ctx, client, baseURL, fromPub, signature)
		if err != nil {
			return Outcome{Kind: KindUnconfirmed, Err: err, Detail: verifyDetail(err)}
		}
		if found {
			return finishFiles(ctx, client, baseURL, fromPub, body, opt, Outcome{Kind: KindDelivered, Found: true})
		}
		absent = true
	}
	err := Message(ctx, client, baseURL, fromPub, signature, body)
	if err == nil {
		return finishFiles(ctx, client, baseURL, fromPub, body, opt, Outcome{Kind: KindDelivered, Absent: absent})
	}
	if IsAmbiguous(err) && ctx.Err() == nil {
		found, verr := signatureOnThread(ctx, client, baseURL, fromPub, signature)
		if verr != nil {
			return Outcome{Kind: KindUnconfirmed, Err: err, Detail: verifyDetail(verr)}
		}
		if found {
			return finishFiles(ctx, client, baseURL, fromPub, body, opt, Outcome{Kind: KindDelivered, Found: true})
		}
		return Outcome{Kind: KindPending, Err: err, Detail: AbsentDetail, Absent: true}
	}
	if IsAmbiguous(err) {
		return Outcome{Kind: KindUnconfirmed, Err: err, Detail: UnconfirmedDetail}
	}
	return Outcome{Kind: KindPending, Err: err, Detail: definiteDetail(err), Absent: absent}
}

func finishFiles(ctx context.Context, client *http.Client, baseURL, fromPub string, body []byte, opt DeliverOptions, base Outcome) Outcome {
	if base.Kind != KindDelivered {
		return base
	}
	files, err := canon.FilesIn(string(body))
	if err != nil {
		return Outcome{Kind: KindPending, Err: err, Detail: err.Error(), Absent: base.Absent}
	}
	if len(files) == 0 {
		return base
	}
	if opt.OpenBlob == nil {
		return Outcome{Kind: KindPending, Err: fmt.Errorf("file blobs were not provided"), Detail: "file blobs were not provided", Absent: base.Absent}
	}
	if err := PutFiles(ctx, client, baseURL, fromPub, files, opt.OpenBlob); err != nil {
		if errors.Is(err, ErrBlobsUnsupported) {
			base.FilesSkipped = true
			base.Detail = "peer does not accept files; the text was delivered"
			return base
		}
		var reject *BlobReject
		if errors.As(err, &reject) && reject.Permanent() {
			base.FilesSkipped = true
			base.Detail = reject.Error()
			return base
		}
		return Outcome{Kind: KindPending, Err: err, Detail: err.Error(), Absent: base.Absent}
	}
	return base
}

// PutFiles uploads each blob. The first failure stops the rest.
func PutFiles(ctx context.Context, client *http.Client, baseURL, fromPub string, files []canon.File, open func(sha string) (io.ReadCloser, error)) error {
	for _, f := range files {
		rc, err := open(f.SHA256)
		if err != nil {
			return err
		}
		err = PutBlob(ctx, client, baseURL, fromPub, f.SHA256, f.Size, rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
	}
	return nil
}

// PutBlob POSTs raw file bytes to the peer's blob endpoint.
// A 404 whose body says "not found" is ErrBlobsUnsupported (an older peer).
func PutBlob(ctx context.Context, client *http.Client, baseURL, fromPub, sha string, size int64, r io.Reader) error {
	if client == nil {
		client = PeerClient()
	}
	u := strings.TrimRight(baseURL, "/") + "/" + fromPub + "/blob/" + sha
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, io.LimitReader(r, size))
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return nil
	}
	msg := blobError(resp.StatusCode, raw)
	if resp.StatusCode == http.StatusNotFound && msg == "not found" {
		return ErrBlobsUnsupported
	}
	return &BlobReject{Status: resp.StatusCode, Msg: msg}
}

func blobError(status int, raw []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err == nil && body.Error != "" {
		return body.Error
	}
	msg := strings.TrimSpace(string(raw))
	if msg == "" {
		return fmt.Sprintf("peer returned %d", status)
	}
	return msg
}

// GetBlob GETs raw file bytes from a peer's thread.
// peer is the caller's public key, the same path key as GET /<peer>.
func GetBlob(ctx context.Context, client *http.Client, baseURL, peer, sha string) (io.ReadCloser, error) {
	if client == nil {
		client = PeerClient()
	}
	u := strings.TrimRight(baseURL, "/") + "/" + peer + "/blob/" + sha
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = fmt.Sprintf("peer returned %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return resp.Body, nil
}

// IsAmbiguous reports whether err means the request may have been processed
// and the response was lost. A timeout, a reset while waiting for the
// response, or a closed connection is ambiguous. Connection refused, DNS
// failure, no route, and an HTTP error status are not: those mean the peer
// did not accept the message.
func IsAmbiguous(err error) bool {
	if err == nil {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "awaiting headers"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "timeout"):
		return true
	default:
		return false
	}
}

func signatureOnThread(ctx context.Context, client *http.Client, baseURL, ourPub, signature string) (bool, error) {
	msgs, err := Thread(ctx, client, baseURL, ourPub)
	if err != nil {
		return false, err
	}
	for _, m := range msgs {
		if m.Signature == signature {
			return true, nil
		}
	}
	return false, nil
}

func verifyDetail(err error) string {
	if err == nil || IsAmbiguous(err) {
		return UnconfirmedDetail
	}
	return "could not verify delivery: " + err.Error()
}

func definiteDetail(err error) string {
	if err == nil {
		return "delivery failed"
	}
	return err.Error()
}

// RemoteMessage is one entry from a peer's GET thread.
// Body is the canonical signed JSON when the message has files. Older peers
// ignore it and rebuild a text-only body. Compat marks a text-only copy this
// server signs so an older fetcher still sees the text of a file message.
type RemoteMessage struct {
	ID        int64  `json:"id"`
	From      string `json:"from"`
	Ts        int64  `json:"ts"`
	Text      string `json:"text"`
	Signature string `json:"signature"`
	Body      string `json:"body,omitempty"`
	Compat    bool   `json:"compat,omitempty"`
}

// Thread GETs the peer's copy of the conversation with us.
func Thread(ctx context.Context, client *http.Client, baseURL, ourPub string) ([]RemoteMessage, error) {
	if client == nil {
		client = PeerClient()
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
