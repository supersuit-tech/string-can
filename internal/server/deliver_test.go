package server_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supersuit-tech/string-can/internal/canon"
	"github.com/supersuit-tech/string-can/internal/cli"
	"github.com/supersuit-tech/string-can/internal/push"
	"github.com/supersuit-tech/string-can/internal/server"
	"github.com/supersuit-tech/string-can/internal/store"
)

func TestSendTimeoutConfirmsStoredMessage(t *testing.T) {
	t.Setenv("STRING_CAN_PEER_TIMEOUT", "200ms")
	var mu sync.Mutex
	var sig string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = io.Copy(io.Discard, r.Body)
			mu.Lock()
			sig = r.Header.Get("X-String-Can-Signature")
			mu.Unlock()
			<-r.Context().Done()
			return
		}
		mu.Lock()
		got := sig
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"messages":[{"id":1,"from":"a","ts":1,"text":"hello","signature":"`+got+`"}]}`)
	}))
	t.Cleanup(peer.Close)

	home, peerKey := homeWithContact(t, peer.URL)
	var stdout, stderr bytes.Buffer
	code := cli.RunContext(context.Background(), []string{"send", peerKey, "hello"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d stderr %s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "sent 1" {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "deadline") || strings.Contains(stderr.String(), "awaiting headers") {
		t.Fatalf("raw timeout leaked: %s", stderr.String())
	}
	if msgs := undelivered(t, home); len(msgs) != 0 {
		t.Fatalf("still undelivered: %+v", msgs)
	}
	mu.Lock()
	defer mu.Unlock()
	if sig == "" {
		t.Fatal("peer did not store the signature")
	}
}

func TestSendTimeoutUnconfirmedWhenPeerStaysSilent(t *testing.T) {
	t.Setenv("STRING_CAN_PEER_TIMEOUT", "200ms")
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(peer.Close)

	home, peerKey := homeWithContact(t, peer.URL)
	var stdout, stderr bytes.Buffer
	code := cli.RunContext(context.Background(), []string{"send", peerKey, "hello"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d stderr %s", code, stderr.String())
	}
	want := "sent 1 (unconfirmed — " + push.UnconfirmedDetail + ")"
	if strings.TrimSpace(stdout.String()) != want {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "deadline") || strings.Contains(stdout.String(), "awaiting headers") {
		t.Fatalf("raw timeout leaked: %s", stdout.String())
	}
	status := run(t, home, 0, "status")
	if !strings.Contains(status, `"state":"unconfirmed"`) || !strings.Contains(status, push.UnconfirmedDetail) {
		t.Fatalf("status %s", status)
	}
	if strings.Contains(status, "deadline") || strings.Contains(status, "awaiting headers") {
		t.Fatalf("status leaked timeout: %s", status)
	}
	msgs := undelivered(t, home)
	if len(msgs) != 1 || msgs[0].State != store.StateUnconfirmed || msgs[0].NextAttempt <= time.Now().UnixMilli() {
		t.Fatalf("%+v", msgs)
	}
}

func TestRetryVerifiesUnconfirmedBeforePost(t *testing.T) {
	var posts atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"messages":[{"id":9,"from":"a","ts":1,"text":"booyah","signature":"sig-9"}]}`)
	}))
	t.Cleanup(peer.Close)

	var logs bytes.Buffer
	home := t.TempDir()
	srv := newBareServer(t, home, &logs, peer.Client())
	peerKey := strings.Repeat("ab", 32)
	if err := srv.Store().UpsertContact(store.Contact{
		PublicKey: peerKey,
		Label:     "peer",
		BaseURL:   peer.URL,
	}); err != nil {
		t.Fatal(err)
	}
	id, _, err := srv.Store().InsertMessage(store.Message{
		PeerPub:   peerKey,
		FromPub:   srv.PublicKey(),
		Ts:        1,
		Text:      "booyah",
		Body:      `{"v":1}`,
		Signature: "sig-9",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Store().RecordOutcome(id, store.StateUnconfirmed, push.UnconfirmedDetail, 1, 0); err != nil {
		t.Fatal(err)
	}
	srv.Tick(context.Background())
	if posts.Load() != 0 {
		t.Fatalf("re-posted a message the peer already had: %d", posts.Load())
	}
	if msgs := undelivered(t, home); len(msgs) != 0 {
		t.Fatalf("not marked delivered: %+v", msgs)
	}
	if !strings.Contains(logs.String(), "verified delivered") {
		t.Fatalf("log %q", logs.String())
	}
}

func TestRetryBackoffDoesNotHammer(t *testing.T) {
	var hits atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	t.Cleanup(peer.Close)

	home := t.TempDir()
	srv := newBareServer(t, home, nil, peer.Client())
	peerKey := strings.Repeat("cd", 32)
	if err := srv.Store().UpsertContact(store.Contact{
		PublicKey: peerKey,
		Label:     "peer",
		BaseURL:   peer.URL,
	}); err != nil {
		t.Fatal(err)
	}
	id, _, err := srv.Store().InsertMessage(store.Message{
		PeerPub:   peerKey,
		FromPub:   srv.PublicKey(),
		Ts:        1,
		Text:      "x",
		Body:      `{"v":1}`,
		Signature: "sig-x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Store().RecordOutcome(id, "", "peer returned 500", 1, 0); err != nil {
		t.Fatal(err)
	}
	srv.Tick(context.Background())
	srv.Tick(context.Background())
	if hits.Load() != 1 {
		t.Fatalf("hits %d", hits.Load())
	}
	msgs := undelivered(t, home)
	if len(msgs) != 1 || msgs[0].Attempts != 2 || msgs[0].NextAttempt <= time.Now().UnixMilli() {
		t.Fatalf("%+v", msgs)
	}
}

func TestRetryUnconfirmedDoesNotPostWhenThreadCheckFails(t *testing.T) {
	var posts atomic.Int32
	var gets atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		} else {
			gets.Add(1)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(peer.Close)

	home := t.TempDir()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	srv := newBareServer(t, home, nil, client)
	peerKey := strings.Repeat("ef", 32)
	if err := srv.Store().UpsertContact(store.Contact{
		PublicKey: peerKey,
		Label:     "peer",
		BaseURL:   peer.URL,
	}); err != nil {
		t.Fatal(err)
	}
	id, _, err := srv.Store().InsertMessage(store.Message{
		PeerPub:   peerKey,
		FromPub:   srv.PublicKey(),
		Ts:        1,
		Text:      "x",
		Body:      `{"v":1}`,
		Signature: "sig-silent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Store().RecordOutcome(id, store.StateUnconfirmed, push.UnconfirmedDetail, 1, 0); err != nil {
		t.Fatal(err)
	}
	srv.Tick(context.Background())
	srv.Tick(context.Background())
	if posts.Load() != 0 {
		t.Fatalf("posted while unconfirmed: %d", posts.Load())
	}
	if gets.Load() != 1 {
		t.Fatalf("thread checks %d", gets.Load())
	}
	msgs := undelivered(t, home)
	if len(msgs) != 1 || msgs[0].State != store.StateUnconfirmed || msgs[0].Attempts != 2 {
		t.Fatalf("%+v", msgs)
	}
	if strings.Contains(msgs[0].LastError, "deadline") || strings.Contains(msgs[0].LastError, "awaiting headers") {
		t.Fatalf("last_error %q", msgs[0].LastError)
	}
}

func homeWithContact(t *testing.T, baseURL string) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("STRING_CAN_HOME", home)
	var stdout bytes.Buffer
	if code := cli.RunContext(context.Background(), []string{"pubkey"}, &stdout, io.Discard); code != 0 {
		t.Fatalf("pubkey exit %d", code)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	peerKey := canon.PublicHexFromPrivate(priv)
	st, err := store.Open(filepath.Join(home, "string-can.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertContact(store.Contact{
		PublicKey: peerKey,
		Label:     "peer",
		BaseURL:   baseURL,
	}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return home, peerKey
}

func newBareServer(t *testing.T, home string, logs *bytes.Buffer, client *http.Client) *server.Server {
	t.Helper()
	cfg := server.Config{
		Home:       home,
		Name:       "retry",
		Port:       0,
		Local:      true,
		WakeEvery:  time.Hour,
		HTTPClient: client,
	}
	if logs != nil {
		cfg.Logger = log.New(logs, "", 0)
	}
	srv, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}
