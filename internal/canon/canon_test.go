package canon

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

func TestEncodeHi(t *testing.T) {
	from := strings.Repeat("1", 64)
	got, err := Encode(from, 1731360000000, "hi")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"from":"` + from + `","ts":1731360000000,"text":"hi"}`
	if string(got) != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestEncodeEscapedQuote(t *testing.T) {
	from := strings.Repeat("1", 64)
	got, err := Encode(from, 0, `say "hi"`)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"from":"` + from + `","ts":0,"text":"say \"hi\""}`
	if string(got) != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestSignVerify(t *testing.T) {
	_, privA, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, privB, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	from := PublicHexFromPrivate(privA)
	body, err := Encode(from, 1731360000000, "hi")
	if err != nil {
		t.Fatal(err)
	}
	sig := Sign(privA, body)
	if err := Verify(from, body, sig); err != nil {
		t.Fatalf("A should verify: %v", err)
	}
	if err := Verify(PublicHexFromPrivate(privB), body, sig); err == nil {
		t.Fatal("B should not verify A's signature")
	}
}
