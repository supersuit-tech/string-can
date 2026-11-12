package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestLastErrorMigration(t *testing.T) {
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
INSERT INTO messages (peer_pubkey, from_pubkey, ts, text, body, signature, delivered, nudged)
VALUES (?, ?, 1, 'x', '{}', 'sig', 0, 0)
`, strings.Repeat("1", 64), strings.Repeat("2", 64)); err != nil {
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
	if err := st.SetLastError(1, "later"); err != nil {
		t.Fatal(err)
	}
	msgs, err := st.Undelivered()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].LastError != "later" {
		t.Fatalf("%+v", msgs)
	}
	if err := st.RecordOutcome(1, StateUnconfirmed, "response timed out, will verify", 2, 99); err != nil {
		t.Fatal(err)
	}
	msgs, err = st.Undelivered()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].State != StateUnconfirmed || msgs[0].Attempts != 2 || msgs[0].NextAttempt != 99 {
		t.Fatalf("%+v", msgs)
	}
	if n, err := st.CountUnconfirmed(); err != nil || n != 1 {
		t.Fatalf("unconfirmed %d %v", n, err)
	}
}

func TestPullOnlyMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("ab", 32)
	if _, err := db.Exec(`
CREATE TABLE contacts (
	public_key TEXT PRIMARY KEY,
	label TEXT NOT NULL,
	base_url TEXT NOT NULL,
	stated_name TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
INSERT INTO contacts (public_key, label, base_url, stated_name, created_at)
VALUES (?, 'muse', 'http://127.0.0.1:1', 'muse', 1)
`, key); err != nil {
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
	c, err := st.GetContact(key)
	if err != nil {
		t.Fatal(err)
	}
	if c.PullOnly {
		t.Fatal("existing contacts should default to push")
	}
	c.PullOnly = true
	if err := st.UpsertContact(c); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetContact(key)
	if err != nil || !got.PullOnly {
		t.Fatalf("pull_only after upsert: %+v %v", got, err)
	}
}

func TestPullOnlyClearsPending(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "string-can.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	peer := strings.Repeat("11", 32)
	if err := st.UpsertContact(Contact{PublicKey: peer, Label: "muse", BaseURL: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.InsertMessage(Message{
		PeerPub: peer, FromPub: strings.Repeat("22", 32), Ts: 1, Text: "x", Body: "{}", Signature: "sig",
	}); err != nil {
		t.Fatal(err)
	}
	n, err := st.CountPending()
	if err != nil || n != 1 {
		t.Fatalf("pending %d %v", n, err)
	}
	if err := st.UpsertContact(Contact{PublicKey: peer, Label: "muse", BaseURL: "http://127.0.0.1:1", PullOnly: true}); err != nil {
		t.Fatal(err)
	}
	n, err = st.CountPending()
	if err != nil || n != 0 {
		t.Fatalf("pending after pull-only %d %v", n, err)
	}
}

func TestWakeStoredSeparatelyFromContact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "string-can.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	peer := strings.Repeat("ab", 32)
	const (
		token = "wake-token-should-not-leak"
		url   = "https://hooks.example.com/v1/wake-token-should-not-leak"
	)
	if err := st.UpsertContact(Contact{PublicKey: peer, Label: "bot", BaseURL: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWake(peer, url, token); err != nil {
		t.Fatal(err)
	}
	c, err := st.GetContact(peer)
	if err != nil {
		t.Fatal(err)
	}
	if c.WakeURL != url || !c.WakeTokenSet {
		t.Fatalf("contact wake fields: %+v", c)
	}
	if strings.Contains(c.Label+c.BaseURL+c.StatedName, token) {
		t.Fatal("token copied onto a display field")
	}
	const proxy = "http://proxyuser:s3cret@egress.example:3128"
	if err := st.SetWakeProxy(peer, proxy); err != nil {
		t.Fatal(err)
	}
	gotURL, gotToken, gotProxy, err := st.LoadWake(peer)
	if err != nil || gotURL != url || gotToken != token || gotProxy != proxy {
		t.Fatalf("load %q %q %q %v", gotURL, gotToken, gotProxy, err)
	}
	c, err = st.GetContact(peer)
	if err != nil || c.WakeProxy != proxy {
		t.Fatalf("contact proxy %+v %v", c, err)
	}
	listed, err := st.ListContacts()
	if err != nil || len(listed) != 1 || listed[0].WakeURL != url || !listed[0].WakeTokenSet || listed[0].WakeProxy != proxy {
		t.Fatalf("list %+v %v", listed, err)
	}
	// Relabeling must not wipe the webhook or the token.
	c.Label = "renamed"
	if err := st.UpsertContact(c); err != nil {
		t.Fatal(err)
	}
	gotURL, gotToken, gotProxy, err = st.LoadWake(peer)
	if err != nil || gotURL != url || gotToken != token || gotProxy != proxy {
		t.Fatalf("upsert wiped wake %q %q %q %v", gotURL, gotToken, gotProxy, err)
	}
	if err := st.ClearWake(peer); err != nil {
		t.Fatal(err)
	}
	c, err = st.GetContact(peer)
	if err != nil || c.WakeURL != "" || c.WakeTokenSet || c.WakeProxy != "" {
		t.Fatalf("after clear %+v %v", c, err)
	}
	if _, gotToken, gotProxy, err = st.LoadWake(peer); err != nil || gotToken != "" || gotProxy != "" {
		t.Fatalf("token remained %q proxy %q %v", gotToken, gotProxy, err)
	}
}

func TestWakeMigrationAndRateLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	peer := strings.Repeat("cd", 32)
	if _, err := db.Exec(`
CREATE TABLE contacts (
	public_key TEXT PRIMARY KEY,
	label TEXT NOT NULL,
	base_url TEXT NOT NULL,
	stated_name TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	pull_only INTEGER NOT NULL DEFAULT 0
);
INSERT INTO contacts (public_key, label, base_url, stated_name, created_at, pull_only)
VALUES (?, 'bot', 'http://127.0.0.1:1', 'bot', 1, 0)
`, peer); err != nil {
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
	c, err := st.GetContact(peer)
	if err != nil || c.WakeURL != "" || c.WakeTokenSet {
		t.Fatalf("migrated %+v %v", c, err)
	}
	if err := st.SetWake(peer, "https://hooks.example.com/wake", "sekrit"); err != nil {
		t.Fatal(err)
	}
	const interval int64 = 120_000
	now := int64(1_000_000)
	claimed, err := st.ClaimWake(peer, now, interval)
	if err != nil || !claimed {
		t.Fatalf("first claim %v %v", claimed, err)
	}
	claimed, err = st.ClaimWake(peer, now+interval-1, interval)
	if err != nil || claimed {
		t.Fatalf("early claim %v %v", claimed, err)
	}
	claimed, err = st.ClaimWake(peer, now+interval, interval)
	if err != nil || !claimed {
		t.Fatalf("boundary claim %v %v", claimed, err)
	}
	if err := st.SetWakeProxy(peer, "direct"); err != nil {
		t.Fatal(err)
	}
	if _, _, gotProxy, err := st.LoadWake(peer); err != nil || gotProxy != "direct" {
		t.Fatalf("proxy %q %v", gotProxy, err)
	}
	if err := st.ClearWake(peer); err != nil {
		t.Fatal(err)
	}
	if _, _, gotProxy, err := st.LoadWake(peer); err != nil || gotProxy != "" {
		t.Fatalf("proxy after clear %q %v", gotProxy, err)
	}
	claimed, err = st.ClaimWake(peer, now+interval*2, interval)
	if err != nil || claimed {
		t.Fatalf("claim without url %v %v", claimed, err)
	}
}

func TestWakeProxyMigratesFromExistingWake(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr16.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	peer := strings.Repeat("ef", 32)
	const (
		token = "kept-token"
		url   = "https://hooks.example.com/kept"
	)
	if _, err := db.Exec(`
CREATE TABLE contacts (
	public_key TEXT PRIMARY KEY,
	label TEXT NOT NULL,
	base_url TEXT NOT NULL,
	stated_name TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	pull_only INTEGER NOT NULL DEFAULT 0,
	wake_url TEXT NOT NULL DEFAULT '',
	wake_token TEXT NOT NULL DEFAULT '',
	wake_at INTEGER NOT NULL DEFAULT 0
);
INSERT INTO contacts (public_key, label, base_url, stated_name, created_at, pull_only, wake_url, wake_token, wake_at)
VALUES (?, 'bot', 'http://127.0.0.1:1', 'bot', 1, 0, ?, ?, 5)
`, peer, url, token); err != nil {
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
	gotURL, gotToken, gotProxy, err := st.LoadWake(peer)
	if err != nil || gotURL != url || gotToken != token || gotProxy != "" {
		t.Fatalf("migrated %q %q %q %v", gotURL, gotToken, gotProxy, err)
	}
}
