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
