package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSendFileSizeLimit(t *testing.T) {
	_, peer, _ := setupPullContact(t)
	t.Setenv("STRING_CAN_MAX_FILE_BYTES", "4")
	path := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runCmd(t, "send", "--file", path, peer, "nope")
	if code != 1 || !strings.Contains(stderr, "STRING_CAN_MAX_FILE_BYTES") {
		t.Fatalf("%d %s", code, stderr)
	}
	if got := threadTexts(t, peer); len(got) != 0 {
		t.Fatalf("stored %#v", got)
	}
}

func TestSendFileUsesBaseName(t *testing.T) {
	_, peer, _ := setupPullContact(t)
	dir := filepath.Join(t.TempDir(), "nested")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "deck.txt")
	if err := os.WriteFile(path, []byte("slides"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := runCmd(t, "send", "--file", path, peer, "here"); code != 0 {
		t.Fatalf("send %d %s", code, stderr)
	}
	stdout, stderr, code := runCmd(t, "thread", peer)
	if code != 0 {
		t.Fatalf("thread %d %s", code, stderr)
	}
	if !strings.Contains(stdout, `"name":"deck.txt"`) || strings.Contains(stdout, "nested") {
		t.Fatalf("thread %s", stdout)
	}
	if !strings.Contains(stdout, `"text":"here"`) || !strings.Contains(stdout, `"sha256"`) {
		t.Fatalf("thread %s", stdout)
	}
}

func TestSendAutoFileUsesCap(t *testing.T) {
	_, peer, _ := setupPullContact(t)
	t.Setenv("STRING_CAN_AUTO_CAP", "1")
	path := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := runCmd(t, "send", "--auto", "--file", path, peer, "one"); code != 0 {
		t.Fatalf("first %d %s", code, stderr)
	}
	other := filepath.Join(t.TempDir(), "other.txt")
	if err := os.WriteFile(other, []byte("xyz"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runCmd(t, "send", "--auto", "--file", other, peer, "two")
	if code != 1 || !strings.Contains(stderr, "auto-reply cap") {
		t.Fatalf("second %d %s", code, stderr)
	}
	got := threadTexts(t, peer)
	if len(got) != 1 || got[0] != "one" {
		t.Fatalf("thread %#v", got)
	}
}

func TestSendFileWithoutText(t *testing.T) {
	_, peer, _ := setupPullContact(t)
	path := filepath.Join(t.TempDir(), "only.txt")
	if err := os.WriteFile(path, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := runCmd(t, "send", "--file", path, peer); code != 0 {
		t.Fatalf("send %d %s", code, stderr)
	}
	got := threadTexts(t, peer)
	if len(got) != 1 || got[0] != "" {
		t.Fatalf("thread %#v", got)
	}
}
