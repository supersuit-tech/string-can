package files

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supersuit-tech/string-can/internal/canon"
)

func TestSaveVerifiedRejectsMismatch(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	good := stMustIngest(t, st, "hello")
	if err := st.SaveVerified("notes.txt", good.SHA256, good.Size, bytes.NewReader([]byte("hello"))); err != nil {
		t.Fatal(err)
	}
	err = st.SaveVerified("notes.txt", good.SHA256, good.Size, bytes.NewReader([]byte("hellp")))
	var mismatch *HashMismatch
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("err %v", err)
	}
	if !errorAs(err, &mismatch) {
		t.Fatalf("type %T", err)
	}
	// The good blob is still the original bytes.
	rc, err := st.Open(good.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != "hello" {
		t.Fatalf("blob %q", buf)
	}
}

func TestSavedPathStaysInDir(t *testing.T) {
	peer := strings.Repeat("ab", 32)
	if _, err := SavedPath(t.TempDir(), peer, 3, "../etc/passwd"); err == nil {
		t.Fatal("accepted traversal")
	}
	got, err := SavedPath(t.TempDir(), peer, 3, "deck.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, filepath.Join("files", peer, "3", "deck.pdf")) {
		t.Fatalf("path %s", got)
	}
}

func TestIngestUsesBaseName(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "sub", "deck.txt")
	if err := os.MkdirAll(filepath.Dir(nested), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("slides"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	f, err := st.Ingest(nested, DefaultMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "deck.txt" || f.Size != 6 || f.Type != "text/plain" {
		t.Fatalf("%+v", f)
	}
	if _, err := canon.NormalizeSHA(f.SHA256); err != nil {
		t.Fatal(err)
	}
}

func TestIngestRejectsOversize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Ingest(path, 4)
	if err == nil || !strings.Contains(err.Error(), "STRING_CAN_MAX_FILE_BYTES") {
		t.Fatalf("err %v", err)
	}
	entries, _ := os.ReadDir(st.Dir)
	if len(entries) != 0 {
		t.Fatalf("kept %d blobs", len(entries))
	}
}

func stMustIngest(t *testing.T, st *Store, text string) canon.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := st.Ingest(path, DefaultMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func errorAs(err error, target **HashMismatch) bool {
	if err == nil {
		return false
	}
	hm, ok := err.(*HashMismatch)
	if !ok {
		return false
	}
	*target = hm
	return true
}
