package main

import (
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPubkeySubprocess(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "string-can")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	home := filepath.Join(tmp, "home")
	cmd := exec.Command(bin, "pubkey")
	cmd.Env = append(os.Environ(), "STRING_CAN_HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pubkey: %v\n%s", err, out)
	}
	got := string(out)
	re := regexp.MustCompile(`^string-can public key: [0-9a-f]{64}\n$`)
	if !re.MatchString(got) {
		t.Fatalf("pubkey output %q", got)
	}

	keyPath := filepath.Join(home, "identity.key")
	priv, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(priv) < 32 {
		t.Fatalf("identity.key too short: %d", len(priv))
	}
	seedHex := hex.EncodeToString(priv[:32])
	if strings.Contains(got, seedHex) {
		t.Fatal("pubkey printed private key material")
	}
	if strings.Contains(strings.ToLower(got), "identity.key") {
		t.Fatal("pubkey mentioned the private key file")
	}
}
