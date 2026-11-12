package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supersuit-tech/string-can/internal/canon"
)

const priceText = "ask $0.005, cap $50M, n $1, ticks $0.001 $0.01 $0.30 $0.0017\n"

func TestSendTextFileKeepsDollarFigures(t *testing.T) {
	_, peer, _ := setupPullContact(t)
	path := filepath.Join(t.TempDir(), "reply.txt")
	if err := os.WriteFile(path, []byte(priceText), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runCmd(t, "send", "--auto", "--text-file", path, peer)
	if code != 0 {
		t.Fatalf("send %d %s %s", code, stdout, stderr)
	}
	stdout, stderr, code = runCmd(t, "send", "--text-file="+path, peer)
	if code != 0 {
		t.Fatalf("equals form %d %s %s", code, stdout, stderr)
	}
	stdout, stderr, code = runCmd(t, "send", peer, "--text-file", path)
	if code != 0 {
		t.Fatalf("flag after key %d %s %s", code, stdout, stderr)
	}
	got := threadTexts(t, peer)
	if len(got) != 3 || got[0] != priceText || got[1] != priceText || got[2] != priceText {
		t.Fatalf("thread %#v", got)
	}
}

func TestSendStdinKeepsDollarFigures(t *testing.T) {
	_, peer, _ := setupPullContact(t)
	stdout, stderr, code := runEnv(t, strings.NewReader(priceText), "send", "--auto", peer)
	if code != 0 {
		t.Fatalf("reader %d %s %s", code, stdout, stderr)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(priceText)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	stdout, stderr, code = runEnv(t, r, "send", peer)
	if code != 0 {
		t.Fatalf("pipe %d %s %s", code, stdout, stderr)
	}
	got := threadTexts(t, peer)
	if len(got) != 2 || got[0] != priceText || got[1] != priceText {
		t.Fatalf("thread %#v", got)
	}
}

func TestSendPositionalTextIgnoresStdin(t *testing.T) {
	_, peer, _ := setupPullContact(t)
	stdout, stderr, code := runEnv(t, strings.NewReader("from stdin $50M"), "send", peer, "from argv $0.005")
	if code != 0 {
		t.Fatalf("send %d %s %s", code, stdout, stderr)
	}
	got := threadTexts(t, peer)
	if len(got) != 1 || got[0] != "from argv $0.005" {
		t.Fatalf("thread %#v", got)
	}
}

func TestSendTextFileIgnoresStdin(t *testing.T) {
	_, peer, _ := setupPullContact(t)
	path := filepath.Join(t.TempDir(), "reply.txt")
	if err := os.WriteFile(path, []byte(priceText), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runEnv(t, errReader{}, "send", "--text-file", path, peer)
	if code != 0 {
		t.Fatalf("send %d %s %s", code, stdout, stderr)
	}
	got := threadTexts(t, peer)
	if len(got) != 1 || got[0] != priceText {
		t.Fatalf("thread %#v", got)
	}
}

func TestSendTextFileAutoCap(t *testing.T) {
	_, peer, _ := setupPullContact(t)
	t.Setenv("STRING_CAN_AUTO_CAP", "1")
	path := filepath.Join(t.TempDir(), "reply.txt")
	if err := os.WriteFile(path, []byte(priceText), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := runCmd(t, "send", "--auto", "--text-file", path, peer); code != 0 {
		t.Fatalf("first %d %s", code, stderr)
	}
	if err := os.WriteFile(path, []byte("second $1"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runCmd(t, "send", "--auto", "--text-file", path, peer)
	if code != 1 || !strings.Contains(stderr, "auto-reply cap") {
		t.Fatalf("second %d %s", code, stderr)
	}
	got := threadTexts(t, peer)
	if len(got) != 1 || got[0] != priceText {
		t.Fatalf("thread %#v", got)
	}
}

func TestSendTextSourcesRejectBadInput(t *testing.T) {
	_, peer, _ := setupPullContact(t)
	path := filepath.Join(t.TempDir(), "reply.txt")
	if err := os.WriteFile(path, []byte(priceText), 0o600); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runCmd(t, "send", "--text-file", path, peer, "also argv")
	if code != 1 || !strings.Contains(stderr, "not both") {
		t.Fatalf("both %d %s", code, stderr)
	}
	_, stderr, code = runCmd(t, "send", "--text-file", filepath.Join(t.TempDir(), "missing.txt"), peer)
	if code != 1 || !strings.Contains(stderr, "missing.txt") {
		t.Fatalf("missing %d %s", code, stderr)
	}
	_, stderr, code = runCmd(t, "send", "--text-file=", peer)
	if code != 1 || !strings.Contains(stderr, "path is empty") {
		t.Fatalf("empty flag %d %s", code, stderr)
	}

	empty := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = runCmd(t, "send", "--text-file", empty, peer)
	if code != 1 || !strings.Contains(stderr, "text is empty") {
		t.Fatalf("empty file %d %s", code, stderr)
	}

	bad := filepath.Join(t.TempDir(), "bad.txt")
	if err := os.WriteFile(bad, []byte{0xff, 0xfe}, 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = runCmd(t, "send", "--text-file", bad, peer)
	if code != 1 || !strings.Contains(stderr, "utf-8") {
		t.Fatalf("utf-8 %d %s", code, stderr)
	}

	huge := filepath.Join(t.TempDir(), "huge.txt")
	if err := os.WriteFile(huge, bytes.Repeat([]byte("a"), canon.MaxTextBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = runCmd(t, "send", "--text-file", huge, peer)
	if code != 1 || !strings.Contains(stderr, "too large") {
		t.Fatalf("large %d %s", code, stderr)
	}

	_, stderr, code = runEnv(t, strings.NewReader(""), "send", peer)
	if code != 1 || !strings.Contains(stderr, "text is empty") {
		t.Fatalf("empty stdin %d %s", code, stderr)
	}
	_, stderr, code = runEnv(t, nil, "send", peer)
	if code != 1 || !strings.Contains(stderr, "message missing") {
		t.Fatalf("nil stdin %d %s", code, stderr)
	}
	_, stderr, code = runCmd(t, "send")
	if code != 1 || !strings.Contains(stderr, sendUsageLine) {
		t.Fatalf("usage %d %s", code, stderr)
	}
	if got := threadTexts(t, peer); len(got) != 0 {
		t.Fatalf("stored a rejected message %#v", got)
	}
}

func TestSendStdinRefusesTerminal(t *testing.T) {
	_, peer, _ := setupPullContact(t)
	orig := isTerminal
	isTerminal = func(uintptr) bool { return true }
	t.Cleanup(func() { isTerminal = orig })

	f, err := os.Create(filepath.Join(t.TempDir(), "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(priceText)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	_, stderr, code := runEnv(t, f, "send", "--auto", peer)
	if code != 1 || !strings.Contains(stderr, "message missing") {
		t.Fatalf("%d %s", code, stderr)
	}
	if got := threadTexts(t, peer); len(got) != 0 {
		t.Fatalf("stored %#v", got)
	}
}

func TestSendHelpMentionsTextFile(t *testing.T) {
	stdout, stderr, code := runCmd(t, "send", "--help")
	if code != 0 || !strings.Contains(stdout, "--text-file") || !strings.Contains(stdout, "stdin") {
		t.Fatalf("send help %d %s %s", code, stdout, stderr)
	}
	stdout, stderr, code = runCmd(t, "help")
	if code != 0 || !strings.Contains(stdout, "--text-file") {
		t.Fatalf("help %d %s %s", code, stdout, stderr)
	}
}

func threadTexts(t *testing.T, peer string) []string {
	t.Helper()
	stdout, stderr, code := runCmd(t, "thread", peer)
	if code != 0 {
		t.Fatalf("thread %d %s", code, stderr)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line == "" {
			continue
		}
		var row struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		out = append(out, row.Text)
	}
	return out
}

func runEnv(t *testing.T, in io.Reader, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := (env{stdout: &stdout, stderr: &stderr, stdin: in}).run(context.Background(), args)
	return stdout.String(), stderr.String(), code
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("stdin was read")
}
