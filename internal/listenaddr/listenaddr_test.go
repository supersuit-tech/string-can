package listenaddr

import "testing"

func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost", "LOCALHOST", "::1", "[::1]"} {
		if !IsLoopbackHost(host) {
			t.Fatalf("%q should be loopback", host)
		}
	}
	for _, host := range []string{"100.64.0.1", "muse", "example.com", "10.0.0.1"} {
		if IsLoopbackHost(host) {
			t.Fatalf("%q should not be loopback", host)
		}
	}
}

func TestIsTailscaleIPv4(t *testing.T) {
	if !IsTailscaleIPv4("100.64.0.1") {
		t.Fatal("100.64.0.1 should be tailnet")
	}
	if !IsTailscaleIPv4("100.127.255.255") {
		t.Fatal("100.127.255.255 should be tailnet")
	}
	if IsTailscaleIPv4("100.128.0.1") {
		t.Fatal("100.128.0.1 is outside 100.64/10")
	}
	if IsTailscaleIPv4("127.0.0.1") || IsTailscaleIPv4("muse") {
		t.Fatal("loopback and names are not tailnet IPv4")
	}
}

func TestIsLoopbackURL(t *testing.T) {
	if !IsLoopbackURL("http://127.0.0.1:8741") {
		t.Fatal("expected loopback URL")
	}
	if !IsLoopbackURL("http://localhost:8741") {
		t.Fatal("expected localhost loopback")
	}
	if IsLoopbackURL("http://muse:8741") || IsLoopbackURL("http://100.64.1.2:8741") {
		t.Fatal("tailnet URLs should not be loopback")
	}
}

func TestURL(t *testing.T) {
	if got := URL("muse", 8741); got != "http://muse:8741" {
		t.Fatalf("got %s", got)
	}
	if got := URL("100.64.0.1", 8741); got != "http://100.64.0.1:8741" {
		t.Fatalf("got %s", got)
	}
}
