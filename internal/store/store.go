package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/supersuit-tech/string-can/internal/canon"

	_ "modernc.org/sqlite"
	sqlite "modernc.org/sqlite"
)

const messageCols = `id, peer_pubkey, from_pubkey, ts, text, body, signature, delivered, nudged, last_error`

// Contact is a saved peer.
type Contact struct {
	PublicKey  string
	Label      string
	BaseURL    string
	StatedName string
	PullOnly   bool
	CreatedAt  int64
}

// Message is a stored thread row.
type Message struct {
	ID        int64
	PeerPub   string
	FromPub   string
	Ts        int64
	Text      string
	Body      string
	Signature string
	Delivered bool
	Nudged    bool
	LastError string
}

// Store is the SQLite-backed address book and message log.
type Store struct {
	db *sql.DB
}

// Open opens path with WAL and a busy timeout.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS contacts (
	public_key TEXT PRIMARY KEY,
	label TEXT NOT NULL,
	base_url TEXT NOT NULL,
	stated_name TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	pull_only INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	peer_pubkey TEXT NOT NULL,
	from_pubkey TEXT NOT NULL,
	ts INTEGER NOT NULL,
	text TEXT NOT NULL,
	body TEXT NOT NULL,
	signature TEXT NOT NULL,
	delivered INTEGER NOT NULL,
	nudged INTEGER NOT NULL,
	last_error TEXT NOT NULL DEFAULT '',
	UNIQUE(peer_pubkey, signature)
);
`); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateMessages(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateContacts(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrateMessages(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(messages)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasLastError := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "last_error" {
			hasLastError = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !hasLastError {
		_, err = db.Exec(`ALTER TABLE messages ADD COLUMN last_error TEXT NOT NULL DEFAULT ''`)
		return err
	}
	return nil
}

func migrateContacts(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(contacts)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasPullOnly := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "pull_only" {
			hasPullOnly = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !hasPullOnly {
		_, err = db.Exec(`ALTER TABLE contacts ADD COLUMN pull_only INTEGER NOT NULL DEFAULT 0`)
		return err
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// UpsertContact inserts or replaces a contact.
func (s *Store) UpsertContact(c Contact) error {
	if c.CreatedAt == 0 {
		c.CreatedAt = time.Now().UnixMilli()
	}
	pullOnly := 0
	if c.PullOnly {
		pullOnly = 1
	}
	_, err := s.db.Exec(`
INSERT INTO contacts (public_key, label, base_url, stated_name, created_at, pull_only)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(public_key) DO UPDATE SET
	label = excluded.label,
	base_url = excluded.base_url,
	stated_name = excluded.stated_name,
	pull_only = excluded.pull_only
`, c.PublicKey, c.Label, c.BaseURL, c.StatedName, c.CreatedAt, pullOnly)
	if err != nil {
		return err
	}
	if c.PullOnly {
		_, err = s.db.Exec(`UPDATE messages SET delivered = 1, last_error = '' WHERE peer_pubkey = ? AND delivered = 0`, c.PublicKey)
	}
	return err
}

// GetContact loads a contact by public key.
func (s *Store) GetContact(pub string) (Contact, error) {
	var c Contact
	var pullOnly int
	err := s.db.QueryRow(`
SELECT public_key, label, base_url, stated_name, created_at, pull_only
FROM contacts WHERE public_key = ?
`, pub).Scan(&c.PublicKey, &c.Label, &c.BaseURL, &c.StatedName, &c.CreatedAt, &pullOnly)
	if errors.Is(err, sql.ErrNoRows) {
		return Contact{}, fmt.Errorf("not a contact")
	}
	c.PullOnly = pullOnly != 0
	return c, err
}

// IsContact reports whether pub is in the address book.
func (s *Store) IsContact(pub string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(1) FROM contacts WHERE public_key = ?`, pub).Scan(&n)
	return n > 0, err
}

// ListContacts returns contacts oldest first.
func (s *Store) ListContacts() ([]Contact, error) {
	rows, err := s.db.Query(`
SELECT public_key, label, base_url, stated_name, created_at, pull_only
FROM contacts ORDER BY created_at ASC, public_key ASC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Contact
	for rows.Next() {
		var c Contact
		var pullOnly int
		if err := rows.Scan(&c.PublicKey, &c.Label, &c.BaseURL, &c.StatedName, &c.CreatedAt, &pullOnly); err != nil {
			return nil, err
		}
		c.PullOnly = pullOnly != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// RenameContact changes only the local label.
func (s *Store) RenameContact(pub, label string) (Contact, error) {
	res, err := s.db.Exec(`UPDATE contacts SET label = ? WHERE public_key = ?`, label, pub)
	if err != nil {
		return Contact{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Contact{}, err
	}
	if n == 0 {
		return Contact{}, fmt.Errorf("not a contact")
	}
	return s.GetContact(pub)
}

// InsertMessage stores a message. duplicate is true when the signature is already present.
func (s *Store) InsertMessage(m Message) (id int64, duplicate bool, err error) {
	delivered := 0
	if m.Delivered {
		delivered = 1
	}
	nudged := 0
	if m.Nudged {
		nudged = 1
	}
	res, err := s.db.Exec(`
INSERT INTO messages (peer_pubkey, from_pubkey, ts, text, body, signature, delivered, nudged, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
`, m.PeerPub, m.FromPub, m.Ts, m.Text, m.Body, m.Signature, delivered, nudged, m.LastError)
	if err != nil {
		if isUnique(err) {
			err = s.db.QueryRow(`
SELECT id FROM messages WHERE peer_pubkey = ? AND signature = ?
`, m.PeerPub, m.Signature).Scan(&id)
			return id, true, err
		}
		return 0, false, err
	}
	id, err = res.LastInsertId()
	return id, false, err
}

// ListThread returns messages for peer with id > since, oldest first.
func (s *Store) ListThread(peer string, since int64) ([]Message, error) {
	rows, err := s.db.Query(`
SELECT `+messageCols+`
FROM messages
WHERE peer_pubkey = ? AND id > ?
ORDER BY id ASC
`, peer, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// VerifiedThread is ListThread with rows whose signatures do not verify omitted.
func (s *Store) VerifiedThread(peer string, since int64) ([]Message, error) {
	msgs, err := s.ListThread(peer, since)
	if err != nil {
		return nil, err
	}
	var out []Message
	for _, m := range msgs {
		if err := canon.Verify(m.FromPub, []byte(m.Body), m.Signature); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// Undelivered returns outbound rows that the peer has not accepted.
func (s *Store) Undelivered() ([]Message, error) {
	rows, err := s.db.Query(`
SELECT ` + messageCols + `
FROM messages
WHERE delivered = 0
ORDER BY id ASC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// UnnudgedInbound returns inbound rows that have not yet produced a webhook POST.
func (s *Store) UnnudgedInbound() ([]Message, error) {
	rows, err := s.db.Query(`
SELECT ` + messageCols + `
FROM messages
WHERE nudged = 0 AND peer_pubkey = from_pubkey
ORDER BY id ASC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// MarkDelivered sets delivered = 1 and clears last_error.
func (s *Store) MarkDelivered(id int64) error {
	_, err := s.db.Exec(`UPDATE messages SET delivered = 1, last_error = '' WHERE id = ?`, id)
	return err
}

// SetLastError records the last delivery error for an outbound row.
func (s *Store) SetLastError(id int64, msg string) error {
	_, err := s.db.Exec(`UPDATE messages SET last_error = ? WHERE id = ?`, msg, id)
	return err
}

// MarkNudged sets nudged = 1.
func (s *Store) MarkNudged(id int64) error {
	_, err := s.db.Exec(`UPDATE messages SET nudged = 1 WHERE id = ?`, id)
	return err
}

// CountMessages returns the number of stored message rows.
func (s *Store) CountMessages() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n)
	return n, err
}

// CountPending returns the number of undelivered outbound rows.
func (s *Store) CountPending() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE delivered = 0`).Scan(&n)
	return n, err
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanMessage(rows *sql.Rows) (Message, error) {
	var m Message
	var delivered, nudged int
	err := rows.Scan(&m.ID, &m.PeerPub, &m.FromPub, &m.Ts, &m.Text, &m.Body, &m.Signature, &delivered, &nudged, &m.LastError)
	m.Delivered = delivered != 0
	m.Nudged = nudged != 0
	return m, err
}

func isUnique(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() == 19 || se.Code() == 2067 || se.Code()&0xff == 19
	}
	return strings.Contains(strings.ToLower(err.Error()), "unique")
}
