package store

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/supersuit-tech/string-can/internal/canon"

	_ "modernc.org/sqlite"
)

func TestInboxCursor(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "string-can.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	peer := stringsRepeatKey('a')
	in, err := st.InboxState(peer)
	if err != nil {
		t.Fatal(err)
	}
	if in != (Inbox{}) {
		t.Fatalf("zero cursor %+v", in)
	}
	if err := st.SetWatermark(peer, 5); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWatermark(peer, 3); err != nil {
		t.Fatal(err)
	}
	in, err = st.InboxState(peer)
	if err != nil || in.Watermark != 5 {
		t.Fatalf("watermark %+v %v", in, err)
	}
	if err := st.RecordAutoSend(peer, "first", 10); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordAutoSend(peer, "second", 20); err != nil {
		t.Fatal(err)
	}
	in, err = st.InboxState(peer)
	if err != nil || in.Watermark != 5 || in.AutoStreak != 2 || in.AutoAt != 20 || in.AutoText != "second" {
		t.Fatalf("after auto %+v %v", in, err)
	}
	if err := st.SetWatermark(peer, 9); err != nil {
		t.Fatal(err)
	}
	if err := st.ResetAuto(peer); err != nil {
		t.Fatal(err)
	}
	in, err = st.InboxState(peer)
	if err != nil || in.Watermark != 9 || in.AutoStreak != 0 || in.AutoAt != 0 || in.AutoText != "" {
		t.Fatalf("after reset %+v %v", in, err)
	}
}

func TestInboundSinceSkipsOursAndBadSignatures(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "string-can.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, peerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, ourPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	peer := canon.PublicHexFromPrivate(peerPriv)
	ours := canon.PublicHexFromPrivate(ourPriv)
	if _, _, err := st.InsertMessage(signedMessage(t, peer, ourPriv, 10, "ours")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.InsertMessage(signedMessage(t, peer, peerPriv, 11, "hi")); err != nil {
		t.Fatal(err)
	}
	bad := signedMessage(t, peer, peerPriv, 12, "forged")
	bad.Signature = "not-a-signature"
	if _, _, err := st.InsertMessage(bad); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.InsertMessage(signedMessage(t, peer, peerPriv, 13, "again")); err != nil {
		t.Fatal(err)
	}
	got, err := st.InboundSince(peer, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != "hi" || got[1].Text != "again" {
		t.Fatalf("%+v", got)
	}
	if got[0].FromPub != peer || got[0].FromPub == ours {
		t.Fatalf("from %+v", got[0])
	}
	got, err = st.InboundSince(peer, got[0].ID)
	if err != nil || len(got) != 1 || got[0].Text != "again" {
		t.Fatalf("since %+v %v", got, err)
	}
}

func TestInboxTableOnOldDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
CREATE TABLE contacts (
	public_key TEXT PRIMARY KEY,
	label TEXT NOT NULL,
	base_url TEXT NOT NULL,
	stated_name TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	peer_pubkey TEXT NOT NULL,
	from_pubkey TEXT NOT NULL,
	ts INTEGER NOT NULL,
	text TEXT NOT NULL,
	body TEXT NOT NULL,
	signature TEXT NOT NULL,
	delivered INTEGER NOT NULL,
	nudged INTEGER NOT NULL,
	UNIQUE(peer_pubkey, signature)
);
`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	peer := stringsRepeatKey('b')
	if err := st.SetWatermark(peer, 4); err != nil {
		t.Fatal(err)
	}
	in, err := st.InboxState(peer)
	if err != nil || in.Watermark != 4 {
		t.Fatalf("%+v %v", in, err)
	}
}

func signedMessage(t *testing.T, peer string, from ed25519.PrivateKey, ts int64, text string) Message {
	t.Helper()
	pub := canon.PublicHexFromPrivate(from)
	body, err := canon.Encode(pub, ts, text)
	if err != nil {
		t.Fatal(err)
	}
	return Message{
		PeerPub:   peer,
		FromPub:   pub,
		Ts:        ts,
		Text:      text,
		Body:      string(body),
		Signature: canon.Sign(from, body),
		Delivered: true,
		Nudged:    true,
	}
}

func stringsRepeatKey(b byte) string {
	return string(bytesRepeat(b, 64))
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
