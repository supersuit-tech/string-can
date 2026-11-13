package canon

import (
	"strings"
	"testing"
)

func TestEncodeWithFiles(t *testing.T) {
	from := strings.Repeat("1", 64)
	sum := strings.Repeat("ab", 32)
	got, err := EncodeMessage(from, 1731360000000, "hi", []File{{
		Name:   "deck.pdf",
		Size:   4,
		Type:   "application/pdf",
		SHA256: strings.ToUpper(sum),
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"from":"` + from + `","ts":1731360000000,"text":"hi","files":[{"name":"deck.pdf","size":4,"type":"application/pdf","sha256":"` + sum + `"}]}`
	if string(got) != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	msg, err := ParseAndCheck(got, from)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Files) != 1 || msg.Files[0].SHA256 != sum || msg.Text != "hi" {
		t.Fatalf("%+v", msg)
	}
}

func TestEncodeEmptyTextRequiresFiles(t *testing.T) {
	from := strings.Repeat("1", 64)
	if _, err := EncodeMessage(from, 1, "", nil); err == nil || !strings.Contains(err.Error(), "text is empty") {
		t.Fatalf("err %v", err)
	}
	sum := strings.Repeat("cd", 32)
	got, err := EncodeMessage(from, 1, "", []File{{
		Name: "a.txt", Size: 0, Type: "text/plain", SHA256: sum,
	}})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := ParseAndCheck(got, from)
	if err != nil || msg.Text != "" || len(msg.Files) != 1 {
		t.Fatalf("msg %+v err %v", msg, err)
	}
}

func TestSafeNameRejectsTraversal(t *testing.T) {
	bad := []string{
		"", ".", "..", "../etc/passwd", "..\\evil", `foo/bar`, `foo\bar`,
		"a\x00b", "has\nline", "has\r", strings.Repeat("a", 256),
	}
	for _, name := range bad {
		if _, err := SafeName(name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	for _, name := range []string{"deck.pdf", "deck.pptx", "notes (1).txt", "a..b"} {
		got, err := SafeName(name)
		if err != nil || got != name {
			t.Fatalf("%q: %v %q", name, err, got)
		}
	}
}

func TestSignedNameMustAlreadyBeSafe(t *testing.T) {
	from := strings.Repeat("1", 64)
	sum := strings.Repeat("ab", 32)
	raw := `{"v":1,"from":"` + from + `","ts":1,"text":"hi","files":[{"name":"../evil.txt","size":1,"type":"text/plain","sha256":"` + sum + `"}]}`
	if _, err := ParseAndCheck([]byte(raw), from); err == nil || !strings.Contains(err.Error(), "invalid file name") {
		t.Fatalf("err %v", err)
	}
}
