package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supersuit-tech/string-can/internal/canon"
	"github.com/supersuit-tech/string-can/internal/home"
	"github.com/supersuit-tech/string-can/internal/store"

	_ "modernc.org/sqlite"
)

func TestInboxReturnsOnlyNewInbound(t *testing.T) {
	dir, peer, peerPriv := setupPullContact(t)
	if _, stderr, code := runCmd(t, "send", peer, "ours"); code != 0 {
		t.Fatalf("send %d %s", code, stderr)
	}
	insertInbound(t, dir, peer, peerPriv, 20, "hello", true)
	insertInbound(t, dir, peer, peerPriv, 21, "forged", false)
	insertInbound(t, dir, peer, peerPriv, 22, "second", true)

	stdout, stderr, code := runCmd(t, "inbox")
	if code != 0 {
		t.Fatalf("inbox %d %s %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "wrote reply rules") {
		t.Fatalf("stderr %s", stderr)
	}
	lines := decodeInbox(t, stdout)
	if len(lines) != 2 || lines[0].Text != "hello" || lines[1].Text != "second" {
		t.Fatalf("%+v", lines)
	}
	if lines[0].Reply != "ok" || lines[0].Reason != "" || lines[0].Peer != peer || lines[0].From != peer {
		t.Fatalf("%+v", lines[0])
	}
	if strings.Contains(stdout, "ours") || strings.Contains(stdout, "forged") {
		t.Fatalf("filtered text leaked %s", stdout)
	}
	rules, err := os.ReadFile(filepath.Join(dir, "reply.rules"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rules), "Never share:") || !strings.Contains(string(rules), "The default is to reply.") {
		t.Fatalf("rules %s", rules)
	}

	stdout, stderr, code = runCmd(t, "inbox")
	if code != 0 || strings.TrimSpace(stdout) != "" {
		t.Fatalf("second inbox %d %q %s", code, stdout, stderr)
	}
	if strings.Contains(stderr, "wrote reply rules") {
		t.Fatalf("rewrote rules %s", stderr)
	}

	insertInbound(t, dir, peer, peerPriv, 30, "later", true)
	stdout, _, code = runCmd(t, "inbox", peer)
	if code != 0 {
		t.Fatal(code)
	}
	lines = decodeInbox(t, stdout)
	if len(lines) != 1 || lines[0].Text != "later" {
		t.Fatalf("%+v", lines)
	}
}

func TestInboxPeekDoesNotAdvance(t *testing.T) {
	dir, peer, peerPriv := setupPullContact(t)
	insertInbound(t, dir, peer, peerPriv, 20, "hello", true)
	first, _, code := runCmd(t, "inbox", "--peek")
	if code != 0 {
		t.Fatal(code)
	}
	second, _, code := runCmd(t, "inbox", "--peek")
	if code != 0 || first != second {
		t.Fatalf("peek changed %q %q", first, second)
	}
	if _, _, code := runCmd(t, "inbox"); code != 0 {
		t.Fatal(code)
	}
	empty, _, code := runCmd(t, "inbox", "--peek")
	if code != 0 || strings.TrimSpace(empty) != "" {
		t.Fatalf("after consume %q", empty)
	}
}

func TestInboxEchoAndCap(t *testing.T) {
	dir, peer, peerPriv := setupPullContact(t)
	t.Setenv("STRING_CAN_AUTO_CAP", "1")
	if _, stderr, code := runCmd(t, "send", "--auto", peer, "ping"); code != 0 {
		t.Fatalf("auto %d %s", code, stderr)
	}
	insertInbound(t, dir, peer, peerPriv, 20, "ping", true)
	stdout, _, code := runCmd(t, "inbox")
	if code != 0 {
		t.Fatal(code)
	}
	lines := decodeInbox(t, stdout)
	if len(lines) != 1 || lines[0].Reply != "hold" || lines[0].Reason != "echo" {
		t.Fatalf("%+v", lines)
	}
	empty, _, code := runCmd(t, "inbox")
	if code != 0 || strings.TrimSpace(empty) != "" {
		t.Fatalf("echo not consumed %q", empty)
	}

	insertInbound(t, dir, peer, peerPriv, 21, "and now?", true)
	stdout, _, code = runCmd(t, "inbox")
	if code != 0 {
		t.Fatal(code)
	}
	lines = decodeInbox(t, stdout)
	if len(lines) != 1 || lines[0].Reply != "hold" || lines[0].Reason != "auto-reply cap" {
		t.Fatalf("%+v", lines)
	}
	if _, stderr, code := runCmd(t, "send", "--auto", peer, "nope"); code != 1 || !strings.Contains(stderr, "auto-reply cap") {
		t.Fatalf("send --auto %d %s", code, stderr)
	}
	thread, _, code := runCmd(t, "thread", peer)
	if code != 0 || strings.Contains(thread, "nope") {
		t.Fatalf("refused send was stored %s", thread)
	}
}

func TestAutoCapResetsOnOperatorSend(t *testing.T) {
	_, peer, _ := setupPullContact(t)
	t.Setenv("STRING_CAN_AUTO_CAP", "1")
	if _, stderr, code := runCmd(t, "send", "--auto", peer, "one"); code != 0 {
		t.Fatalf("first %d %s", code, stderr)
	}
	if _, stderr, code := runCmd(t, "send", "--auto", peer, "two"); code != 1 || !strings.Contains(stderr, "auto-reply cap") {
		t.Fatalf("second %d %s", code, stderr)
	}
	if _, stderr, code := runCmd(t, "send", peer, "operator"); code != 0 {
		t.Fatalf("operator %d %s", code, stderr)
	}
	if _, stderr, code := runCmd(t, "send", "--auto", peer, "three"); code != 0 {
		t.Fatalf("after reset %d %s", code, stderr)
	}
}

func TestInboxCooldownDoesNotAdvance(t *testing.T) {
	dir, peer, peerPriv := setupPullContact(t)
	t.Setenv("STRING_CAN_AUTO_COOLDOWN", "1h")
	if _, stderr, code := runCmd(t, "send", "--auto", peer, "ping"); code != 0 {
		t.Fatalf("auto %d %s", code, stderr)
	}
	if _, stderr, code := runCmd(t, "send", "--auto", peer, "again"); code != 1 || !strings.Contains(stderr, "cooldown") {
		t.Fatalf("cooldown send %d %s", code, stderr)
	}
	insertInbound(t, dir, peer, peerPriv, 20, "question", true)
	insertInbound(t, dir, peer, peerPriv, 21, "ping", true)
	first, _, code := runCmd(t, "inbox")
	if code != 0 {
		t.Fatal(code)
	}
	lines := decodeInbox(t, first)
	if len(lines) != 2 || lines[0].Reason != "cooldown" || lines[1].Reason != "echo" {
		t.Fatalf("%+v", lines)
	}
	second, _, code := runCmd(t, "inbox")
	if code != 0 || first != second {
		t.Fatalf("cooldown advanced %q %q", first, second)
	}
	clearAutoAt(t, dir, peer)
	stdout, _, code := runCmd(t, "inbox")
	if code != 0 {
		t.Fatal(code)
	}
	lines = decodeInbox(t, stdout)
	if len(lines) != 2 || lines[0].Text != "question" || lines[0].Reply != "ok" || lines[1].Reason != "echo" {
		t.Fatalf("after gap %+v", lines)
	}
	empty, _, code := runCmd(t, "inbox")
	if code != 0 || strings.TrimSpace(empty) != "" {
		t.Fatalf("not consumed %q", empty)
	}
}

func TestInboxLockAndRulesEdit(t *testing.T) {
	dir, peer, peerPriv := setupPullContact(t)
	lk, err := (home.Dir{Path: dir}).InboxLock()
	if err != nil {
		t.Fatal(err)
	}
	insertInbound(t, dir, peer, peerPriv, 20, "hello", true)
	_, stderr, code := runCmd(t, "inbox")
	if code != 1 || !strings.Contains(stderr, "inbox already running") {
		t.Fatalf("locked inbox %d %s", code, stderr)
	}
	stdout, _, code := runCmd(t, "inbox", "--peek")
	if code != 0 || !strings.Contains(stdout, "hello") {
		t.Fatalf("peek while locked %d %s", code, stdout)
	}
	lk.Unlock()
	if err := os.WriteFile(filepath.Join(dir, "reply.rules"), []byte("operator rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runCmd(t, "inbox"); code != 0 {
		t.Fatal(code)
	}
	got, err := os.ReadFile(filepath.Join(dir, "reply.rules"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "operator rules\n" {
		t.Fatalf("rules overwritten %q", got)
	}
}

func TestInboxWatch(t *testing.T) {
	dir, peer, peerPriv := setupPullContact(t)
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- RunContext(ctx, []string{"inbox", "--watch", "--every", "30ms"}, &stdout, &stderr)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(stderr.String(), "wrote reply rules") {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(stderr.String(), "wrote reply rules") {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		t.Fatalf("watch did not start %s", stderr.String())
	}
	insertInbound(t, dir, peer, peerPriv, 20, "watched", true)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(stdout.String(), `"text":"watched"`) {
		time.Sleep(20 * time.Millisecond)
	}
	insertInbound(t, dir, peer, peerPriv, 21, "second", true)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(stdout.String(), `"text":"second"`) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("watch exit %d %s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("inbox --watch did not exit")
	}
	if !strings.Contains(stdout.String(), `"text":"watched"`) || !strings.Contains(stdout.String(), `"text":"second"`) {
		t.Fatalf("watch stdout %s stderr %s", stdout.String(), stderr.String())
	}
}

func TestWatchDoesNotReprintCooldown(t *testing.T) {
	dir, peer, peerPriv := setupPullContact(t)
	t.Setenv("STRING_CAN_AUTO_COOLDOWN", "1h")
	if _, stderr, code := runCmd(t, "send", "--auto", peer, "ping"); code != 0 {
		t.Fatalf("auto %d %s", code, stderr)
	}
	insertInbound(t, dir, peer, peerPriv, 20, "question", true)
	st, err := store.Open(filepath.Join(dir, "string-can.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	suppress := map[int64]bool{}
	var stdout bytes.Buffer
	e := env{stdout: &stdout, stderr: io.Discard}
	n, code := e.inboxOnce(st, nil, true, time.Now(), suppress)
	if code != 0 || n != 1 || !strings.Contains(stdout.String(), `"reason":"cooldown"`) {
		t.Fatalf("first n=%d code=%d %s", n, code, stdout.String())
	}
	stdout.Reset()
	n, code = e.inboxOnce(st, nil, true, time.Now(), suppress)
	if code != 0 || n != 0 || stdout.Len() != 0 {
		t.Fatalf("reprinted n=%d code=%d %s", n, code, stdout.String())
	}
	in, err := st.InboxState(peer)
	if err != nil || in.Watermark != 0 {
		t.Fatalf("watermark moved %+v %v", in, err)
	}
}

func TestInboxPeerFilterAndFlags(t *testing.T) {
	dir, peer, peerPriv := setupPullContact(t)
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other := canon.PublicHexFromPrivate(otherPriv)
	st, err := store.Open(filepath.Join(dir, "string-can.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertContact(store.Contact{PublicKey: other, Label: "other", PullOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	insertInbound(t, dir, peer, peerPriv, 20, "from-peer", true)
	insertInbound(t, dir, other, otherPriv, 21, "from-other", true)

	stdout, _, code := runCmd(t, "inbox", peer)
	if code != 0 {
		t.Fatal(code)
	}
	lines := decodeInbox(t, stdout)
	if len(lines) != 1 || lines[0].Text != "from-peer" {
		t.Fatalf("%+v", lines)
	}
	stdout, _, code = runCmd(t, "inbox")
	lines = decodeInbox(t, stdout)
	if code != 0 || len(lines) != 1 || lines[0].Text != "from-other" {
		t.Fatalf("%d %+v", code, lines)
	}

	stdout, _, code = runCmd(t, "inbox", "--help")
	if code != 0 || !strings.Contains(stdout, "inbox") || !strings.Contains(stdout, "--peek") {
		t.Fatalf("help %d %s", code, stdout)
	}
	_, stderr, code := runCmd(t, "inbox", "--watch", "--peek")
	if code != 1 || !strings.Contains(stderr, "--peek") {
		t.Fatalf("peek+watch %d %s", code, stderr)
	}
	_, stderr, code = runCmd(t, "inbox", strings.Repeat("ab", 32))
	if code != 1 || !strings.Contains(stderr, "not a contact") {
		t.Fatalf("unknown peer %d %s", code, stderr)
	}
	t.Setenv("STRING_CAN_AUTO_CAP", "no")
	_, stderr, code = runCmd(t, "inbox")
	if code != 1 || !strings.Contains(stderr, "STRING_CAN_AUTO_CAP") {
		t.Fatalf("bad cap %d %s", code, stderr)
	}
}

func TestDoctorReportsReplyLoop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STRING_CAN_HOME", dir)
	t.Setenv("STRING_CAN_AUTO_CAP", "3")
	t.Setenv("STRING_CAN_AUTO_COOLDOWN", "")
	stdout, stderr, code := runCmd(t, "doctor")
	if !strings.Contains(stdout, "reply: auto cap 3, cooldown off, rules not written yet") {
		t.Fatalf("doctor %d %s %s", code, stdout, stderr)
	}
}

func setupPullContact(t *testing.T) (dir, peer string, peerPriv ed25519.PrivateKey) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("STRING_CAN_HOME", dir)
	t.Setenv("STRING_CAN_AUTO_CAP", "")
	t.Setenv("STRING_CAN_AUTO_COOLDOWN", "")
	if _, stderr, code := runCmd(t, "pubkey"); code != 0 {
		t.Fatalf("pubkey %d %s", code, stderr)
	}
	var err error
	_, peerPriv, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	peer = canon.PublicHexFromPrivate(peerPriv)
	st, err := store.Open(filepath.Join(dir, "string-can.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertContact(store.Contact{PublicKey: peer, Label: "peer", PullOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, peer, peerPriv
}

func insertInbound(t *testing.T, dir, peer string, priv ed25519.PrivateKey, ts int64, text string, good bool) {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "string-can.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pub := canon.PublicHexFromPrivate(priv)
	body, err := canon.Encode(pub, ts, text)
	if err != nil {
		t.Fatal(err)
	}
	sig := canon.Sign(priv, body)
	if !good {
		sig = "not-a-signature"
	}
	if _, _, err := st.InsertMessage(store.Message{
		PeerPub:   peer,
		FromPub:   pub,
		Ts:        ts,
		Text:      text,
		Body:      string(body),
		Signature: sig,
		Delivered: true,
		Nudged:    true,
	}); err != nil {
		t.Fatal(err)
	}
}

func clearAutoAt(t *testing.T, dir, peer string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "string-can.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	res, err := db.Exec(`UPDATE inbox SET auto_at = 0 WHERE peer_pubkey = ?`, peer)
	if err != nil {
		t.Fatal(err)
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		t.Fatalf("auto_at rows %d %v", n, err)
	}
}

type inboxJSON struct {
	ID     int64  `json:"id"`
	Peer   string `json:"peer"`
	From   string `json:"from"`
	Ts     int64  `json:"ts"`
	Text   string `json:"text"`
	Reply  string `json:"reply"`
	Reason string `json:"reason"`
}

func decodeInbox(t *testing.T, stdout string) []inboxJSON {
	t.Helper()
	var out []inboxJSON
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line == "" {
			continue
		}
		var got inboxJSON
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		out = append(out, got)
	}
	return out
}

func runCmd(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunContext(context.Background(), args, &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}
