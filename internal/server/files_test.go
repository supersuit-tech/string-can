package server_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/supersuit-tech/string-can/internal/canon"
	"github.com/supersuit-tech/string-can/internal/cli"
)

func TestPushFilesRoundTrip(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	a := startServer(t, homeA, "alpha", 0, "", "")
	b := startServer(t, homeB, "beta", 0, "", "")
	run(t, homeA, 0, "connect", "--local", b.BaseURL())
	run(t, homeB, 0, "connect", "--local", a.BaseURL())

	note := filepath.Join(t.TempDir(), "nested", "notes.txt")
	deck := filepath.Join(filepath.Dir(note), "deck.txt")
	if err := os.MkdirAll(filepath.Dir(note), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(note, []byte("hello notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deck, []byte("slides\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := run(t, homeA, 0, "send", "--file", note, "--file", deck, b.PublicKey(), "see attached")
	if !strings.Contains(out, "sent ") || strings.Contains(out, "pending") {
		t.Fatalf("send %q", out)
	}

	threadB := run(t, homeB, 0, "thread", a.PublicKey())
	if !strings.Contains(threadB, `"text":"see attached"`) || !strings.Contains(threadB, `"name":"notes.txt"`) || !strings.Contains(threadB, `"name":"deck.txt"`) {
		t.Fatalf("thread %s", threadB)
	}
	if strings.Contains(threadB, "nested") || strings.Contains(threadB, `"path"`) {
		t.Fatalf("path leaked before download %s", threadB)
	}
	noteSum := shaHex([]byte("hello notes\n"))
	deckSum := shaHex([]byte("slides\n"))
	if !strings.Contains(threadB, noteSum) || !strings.Contains(threadB, deckSum) {
		t.Fatalf("hashes missing %s", threadB)
	}
	// Push uploaded the bytes. B did not fetch.
	if _, err := os.Stat(filepath.Join(homeB, "blobs", noteSum)); err != nil {
		t.Fatalf("pushed blob missing: %v", err)
	}

	inbox := run(t, homeB, 0, "inbox", "--peek")
	if !strings.Contains(inbox, `"attachments"`) || !strings.Contains(inbox, `"sha256":"`+noteSum+`"`) {
		t.Fatalf("inbox %s", inbox)
	}
	id := messageID(t, inbox)

	dl := run(t, homeB, 0, "download", a.PublicKey(), id)
	if !strings.Contains(dl, `"path"`) || !strings.Contains(dl, "notes.txt") {
		t.Fatalf("download %s", dl)
	}
	var saved struct {
		Attachments []struct {
			Name string `json:"name"`
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal([]byte(dl), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Attachments) != 2 {
		t.Fatalf("%+v", saved)
	}
	for _, att := range saved.Attachments {
		info, err := os.Stat(att.Path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 != 0 {
			t.Fatalf("executable %s mode %o", att.Path, info.Mode().Perm())
		}
		body, err := os.ReadFile(att.Path)
		if err != nil {
			t.Fatal(err)
		}
		switch att.Name {
		case "notes.txt":
			if string(body) != "hello notes\n" {
				t.Fatalf("notes %q", body)
			}
		case "deck.txt":
			if string(body) != "slides\n" {
				t.Fatalf("deck %q", body)
			}
		default:
			t.Fatalf("name %s", att.Name)
		}
		if !strings.HasPrefix(att.Path, filepath.Join(homeB, "files", a.PublicKey())) {
			t.Fatalf("path %s", att.Path)
		}
	}
	threadB = run(t, homeB, 0, "thread", a.PublicKey())
	if !strings.Contains(threadB, `"path"`) {
		t.Fatalf("thread after download %s", threadB)
	}
}

func TestPullOnlyFilesRoundTrip(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	a := startServer(t, homeA, "alpha", 0, "", "")
	b := startServer(t, homeB, "beta", 0, "", "")
	// B connects first. A's later --pull-only must win: B's handshake would
	// otherwise clear the pull-only flag on A.
	run(t, homeB, 0, "connect", "--local", a.BaseURL())
	run(t, homeA, 0, "connect", "--local", "--pull-only", b.BaseURL())

	path := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(path, []byte("pulled-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := run(t, homeA, 0, "send", "--file", path, b.PublicKey(), "for fetch")
	if !strings.Contains(out, "(pull)") {
		t.Fatalf("send %q", out)
	}
	sum := shaHex([]byte("pulled-bytes"))
	if _, err := os.Stat(filepath.Join(homeB, "blobs", sum)); !os.IsNotExist(err) {
		t.Fatalf("blob arrived without a fetch: %v", err)
	}
	fetched := run(t, homeB, 0, "fetch", a.PublicKey())
	if !strings.Contains(fetched, `"text":"for fetch"`) || !strings.Contains(fetched, `"name":"note.txt"`) || !strings.Contains(fetched, "fetched 1") {
		t.Fatalf("fetch %s", fetched)
	}
	if strings.Contains(fetched, `"compat"`) {
		t.Fatalf("compat copy was stored %s", fetched)
	}
	if _, err := os.Stat(filepath.Join(homeB, "blobs", sum)); err != nil {
		t.Fatalf("fetch did not pull the blob: %v", err)
	}
	id := messageID(t, run(t, homeB, 0, "thread", a.PublicKey()))
	dl := run(t, homeB, 0, "download", a.PublicKey(), id)
	var saved struct {
		Attachments []struct {
			Path string `json:"path"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal([]byte(dl), &saved); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(saved.Attachments[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "pulled-bytes" {
		t.Fatalf("body %q", body)
	}
}

func TestOutboundOnlyPushesFiles(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	a := startOutbound(t, homeA, "muse", 0)
	b := startServer(t, homeB, "beta", 0, "", "")
	run(t, homeA, 0, "connect", "--local", b.BaseURL())
	run(t, homeB, 0, "connect", "--local", a.BaseURL())

	path := filepath.Join(t.TempDir(), "from-muse.txt")
	if err := os.WriteFile(path, []byte("outbound"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := run(t, homeA, 0, "send", "--file", path, b.PublicKey(), "from muse")
	if !strings.Contains(out, "sent ") || strings.Contains(out, "pending") || strings.Contains(out, "pull") {
		t.Fatalf("send %q", out)
	}
	sum := shaHex([]byte("outbound"))
	if _, err := os.Stat(filepath.Join(homeB, "blobs", sum)); err != nil {
		t.Fatalf("outbound push did not upload the blob: %v", err)
	}
	thread := run(t, homeB, 0, "thread", a.PublicKey())
	if !strings.Contains(thread, `"text":"from muse"`) || !strings.Contains(thread, sum) {
		t.Fatalf("thread %s", thread)
	}
}

func TestOldPeerKeepsText(t *testing.T) {
	var gotText string
	var gotBody []byte
	peerPub := mustKey(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			writeJSON(w, map[string]any{"v": 1, "name": "old", "public_key": peerPub})
			return
		}
		if r.URL.Path == "/connect" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if strings.Contains(r.URL.Path, "/blob/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var m struct {
			From string `json:"from"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(body, &m); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if err := canon.Verify(m.From, body, r.Header.Get("X-String-Can-Signature")); err != nil {
			http.Error(w, "sig", http.StatusUnauthorized)
			return
		}
		gotText = m.Text
		gotBody = append([]byte(nil), body...)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	t.Cleanup(fake.Close)

	homeA := t.TempDir()
	run(t, homeA, 0, "connect", "--local", peerPub, fake.URL)
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runBoth(t, homeA, "send", "--file", path, peerPub, "still the text")
	if code != 0 {
		t.Fatalf("send %d %s %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "sent ") {
		t.Fatalf("stdout %s", stdout)
	}
	if !strings.Contains(stderr, "does not accept files") {
		t.Fatalf("stderr %s", stderr)
	}
	if gotText != "still the text" {
		t.Fatalf("old peer text %q", gotText)
	}
	if !bytes.Contains(gotBody, []byte("sha256")) || !bytes.Contains(gotBody, []byte("notes.txt")) {
		t.Fatalf("signed body %s", gotBody)
	}
}

func TestOldFetcherSeesFileMessageText(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	a := startServer(t, homeA, "alpha", 0, "", "")
	b := startServer(t, homeB, "beta", 0, "", "")
	run(t, homeB, 0, "connect", "--local", a.BaseURL())
	run(t, homeA, 0, "connect", "--local", "--pull-only", b.BaseURL())
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, homeA, 0, "send", "--file", path, b.PublicKey(), "readable")

	resp, err := http.Get(a.BaseURL() + "/" + b.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page struct {
		Messages []struct {
			From      string `json:"from"`
			Ts        int64  `json:"ts"`
			Text      string `json:"text"`
			Signature string `json:"signature"`
			Body      string `json:"body"`
			Compat    bool   `json:"compat"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 {
		t.Fatalf("messages %+v", page.Messages)
	}
	var sawCompat, sawReal bool
	for _, m := range page.Messages {
		if m.Text != "readable" || m.From != a.PublicKey() {
			t.Fatalf("%+v", m)
		}
		plain, err := canon.Encode(m.From, m.Ts, m.Text)
		if err != nil {
			t.Fatal(err)
		}
		plainErr := canon.Verify(m.From, plain, m.Signature)
		if m.Compat {
			sawCompat = true
			if plainErr != nil {
				t.Fatalf("older fetcher would drop the text: %v", plainErr)
			}
			continue
		}
		sawReal = true
		if plainErr == nil {
			t.Fatal("file metadata is not covered by the signature")
		}
		if err := canon.Verify(m.From, []byte(m.Body), m.Signature); err != nil {
			t.Fatalf("real body: %v", err)
		}
		if !strings.Contains(m.Body, shaHex([]byte("abc"))) {
			t.Fatalf("body %s", m.Body)
		}
	}
	if !sawCompat || !sawReal {
		t.Fatalf("compat %v real %v", sawCompat, sawReal)
	}
}

func TestBlobHashMismatchRejected(t *testing.T) {
	homeA := t.TempDir()
	srv := startServer(t, homeA, "alpha", 0, "", "")
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	from := canon.PublicHexFromPrivate(priv)
	mustContact(t, srv.Server, from, srv.BaseURL(), "peer")
	good := []byte("hello")
	sum := shaHex(good)
	body, err := canon.EncodeMessage(from, 10, "hi", []canon.File{{
		Name: "notes.txt", Size: int64(len(good)), Type: "text/plain", SHA256: sum,
	}})
	if err != nil {
		t.Fatal(err)
	}
	sig := canon.Sign(priv, body)
	status, raw := postMessage(t, srv.BaseURL(), from, sig, body)
	if status != http.StatusCreated {
		t.Fatalf("message %d %s", status, raw)
	}
	status, raw = postBytes(t, srv.BaseURL()+"/"+from+"/blob/"+sum, []byte("hellp"))
	if status != http.StatusBadRequest || !bytes.Contains(raw, []byte("hash mismatch")) {
		t.Fatalf("blob %d %s", status, raw)
	}
	if _, err := os.Stat(filepath.Join(homeA, "blobs", sum)); !os.IsNotExist(err) {
		t.Fatalf("mismatch was stored: %v", err)
	}
	status, raw = postBytes(t, srv.BaseURL()+"/"+from+"/blob/"+sum, good)
	if status != http.StatusCreated {
		t.Fatalf("good blob %d %s", status, raw)
	}
	resp, err := http.Get(srv.BaseURL() + "/" + from + "/blob/" + sum)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, good) {
		t.Fatalf("get %d %q", resp.StatusCode, got)
	}

	// A later bad upload must not replace the good blob.
	status, raw = postBytes(t, srv.BaseURL()+"/"+from+"/blob/"+sum, []byte("nope!"))
	if status != http.StatusBadRequest || !bytes.Contains(raw, []byte("hash mismatch")) {
		t.Fatalf("second %d %s", status, raw)
	}
	resp, err = http.Get(srv.BaseURL() + "/" + from + "/blob/" + sum)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ = io.ReadAll(resp.Body)
	if !bytes.Equal(got, good) {
		t.Fatalf("replaced %q", got)
	}
}

func TestBlobFromNonContactRejected(t *testing.T) {
	srv := startServer(t, t.TempDir(), "alpha", 0, "", "")
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	from := canon.PublicHexFromPrivate(priv)
	sum := strings.Repeat("ab", 32)
	status, raw := postBytes(t, srv.BaseURL()+"/"+from+"/blob/"+sum, []byte("x"))
	if status != http.StatusForbidden {
		t.Fatalf("status %d %s", status, raw)
	}
}

func TestUnsafeFileNameRejected(t *testing.T) {
	homeA := t.TempDir()
	srv := startServer(t, homeA, "alpha", 0, "", "")
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	from := canon.PublicHexFromPrivate(priv)
	mustContact(t, srv.Server, from, srv.BaseURL(), "peer")
	sum := strings.Repeat("ab", 32)
	for _, name := range []string{"../evil.txt", `..\evil.txt`, "foo/bar", ".."} {
		payload := struct {
			V     int          `json:"v"`
			From  string       `json:"from"`
			Ts    int64        `json:"ts"`
			Text  string       `json:"text"`
			Files []canon.File `json:"files"`
		}{V: 1, From: from, Ts: 1, Text: "hi", Files: []canon.File{{
			Name: name, Size: 1, Type: "text/plain", SHA256: sum,
		}}}
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		sig := canon.Sign(priv, body)
		status, raw := postMessage(t, srv.BaseURL(), from, sig, body)
		if status != http.StatusBadRequest || !bytes.Contains(raw, []byte("invalid file name")) {
			t.Fatalf("%s: %d %s", name, status, raw)
		}
	}
	if n := countMessages(t, homeA); n != 0 {
		t.Fatalf("stored %d", n)
	}
}

func TestServerRejectsOversizedClaim(t *testing.T) {
	t.Setenv("STRING_CAN_MAX_FILE_BYTES", "4")
	homeA := t.TempDir()
	srv := startServer(t, homeA, "alpha", 0, "", "")
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	from := canon.PublicHexFromPrivate(priv)
	mustContact(t, srv.Server, from, srv.BaseURL(), "peer")
	sum := strings.Repeat("ab", 32)
	body, err := canon.EncodeMessage(from, 3, "hi", []canon.File{{
		Name: "big.txt", Size: 5, Type: "text/plain", SHA256: sum,
	}})
	if err != nil {
		t.Fatal(err)
	}
	status, raw := postMessage(t, srv.BaseURL(), from, canon.Sign(priv, body), body)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d %s", status, raw)
	}
	if n := countMessages(t, homeA); n != 0 {
		t.Fatalf("stored %d", n)
	}
}

func TestDownloadRejectsTamperedBlob(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	a := startServer(t, homeA, "alpha", 0, "", "")
	b := startServer(t, homeB, "beta", 0, "", "")
	run(t, homeA, 0, "connect", "--local", b.BaseURL())
	run(t, homeB, 0, "connect", "--local", a.BaseURL())
	path := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(path, []byte("honest"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, homeA, 0, "send", "--file", path, b.PublicKey(), "check me")
	sum := shaHex([]byte("honest"))
	blob := filepath.Join(homeB, "blobs", sum)
	if err := os.WriteFile(blob, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := messageID(t, run(t, homeB, 0, "thread", a.PublicKey()))
	_, stderr, code := runBoth(t, homeB, "download", a.PublicKey(), id)
	if code == 0 || !strings.Contains(stderr, "hash mismatch") {
		t.Fatalf("download %d %s", code, stderr)
	}
	saved := filepath.Join(homeB, "files", a.PublicKey(), id, "note.txt")
	if _, err := os.Stat(saved); !os.IsNotExist(err) {
		t.Fatalf("saved tampered file: %v", err)
	}
}

func mustKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return canon.PublicHexFromPrivate(priv)
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func messageID(t *testing.T, stdout string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var row struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row.ID > 0 {
			return strconv.FormatInt(row.ID, 10)
		}
	}
	t.Fatalf("no id in %s", stdout)
	return ""
}

func postBytes(t *testing.T, url string, body []byte) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

func runBoth(t *testing.T, homeDir string, args ...string) (string, string, int) {
	t.Helper()
	t.Setenv("STRING_CAN_HOME", homeDir)
	var stdout, stderr bytes.Buffer
	code := cli.RunContext(context.Background(), args, &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
