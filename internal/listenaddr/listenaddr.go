package listenaddr

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultPort is the string-can listen port.
const DefaultPort = 8741

// CGNAT is Tailscale's IPv4 range (100.64.0.0/10).
var cgnat *net.IPNet

func init() {
	_, cgnat, _ = net.ParseCIDR("100.64.0.0/10")
}

// FindTailscaleIPv4 looks up a Tailscale IPv4 address on local interfaces.
// Tests may replace this.
var FindTailscaleIPv4 = lookupTailscaleIPv4

func lookupTailscaleIPv4() (string, bool) {
	if cgnat == nil {
		return "", false
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", false
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			ip4 := ip.To4()
			if ip4 != nil && cgnat.Contains(ip4) {
				return ip4.String(), true
			}
		}
	}
	return "", false
}

// TailscaleIPv4 returns the current Tailscale IPv4 address, if any.
func TailscaleIPv4() (string, bool) {
	return FindTailscaleIPv4()
}

// Host returns the Tailscale IPv4 address if one is present, otherwise 127.0.0.1.
func Host() string {
	if ip, ok := TailscaleIPv4(); ok {
		return ip
	}
	return "127.0.0.1"
}

// URL returns the http base URL for host and port, with no trailing slash.
func URL(host string, port int) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}

// WouldBind is the URL serve would use with the default port and current host.
func WouldBind(port int) string {
	if port == 0 {
		port = DefaultPort
	}
	return URL(Host(), port)
}

// IsTailscaleIPv4 reports whether host is a Tailscale CGNAT IPv4 address.
func IsTailscaleIPv4(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil || cgnat == nil {
		return false
	}
	ip4 := ip.To4()
	return ip4 != nil && cgnat.Contains(ip4)
}

// IsLoopbackHost reports whether host is a loopback name or address.
func IsLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	host = strings.Trim(host, "[]")
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// HostOf returns the hostname from a base URL.
func HostOf(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// IsLoopbackURL reports whether baseURL points at a loopback host.
func IsLoopbackURL(baseURL string) bool {
	return IsLoopbackHost(HostOf(baseURL))
}

// HintAfter is how long WaitForTailscale waits before suggesting --local or
// --outbound-only. Tests may shorten this.
var HintAfter = 30 * time.Second

// NoTailnetHint is printed once after HintAfter while still waiting for 100.x.
const NoTailnetHint = "no tailnet interface found; use --local + fetch if this agent is outbound-only"

// ErrNoTailscale is returned when WaitForTailscale times out or is cancelled
// without finding a 100.x address.
var ErrNoTailscale = fmt.Errorf("no Tailscale IPv4 address; pass --local to bind to 127.0.0.1, or --outbound-only if this agent will poll with fetch")

// WaitForTailscale blocks until a Tailscale IPv4 address appears, ctx is done,
// or wait elapses (wait <= 0 means until ctx is done). onWait is called when
// waiting starts and on each interval tick while still waiting. After HintAfter
// it also emits NoTailnetHint once.
func WaitForTailscale(ctx context.Context, wait, interval time.Duration, onWait func(string)) (string, error) {
	if ip, ok := TailscaleIPv4(); ok {
		return ip, nil
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if onWait != nil {
		onWait("no Tailscale IPv4 found; waiting (pass --local to bind to 127.0.0.1)")
	}
	waitCtx := ctx
	if wait > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	start := time.Now()
	hinted := false
	maybeHint := func() {
		if hinted || HintAfter <= 0 || onWait == nil {
			return
		}
		if time.Since(start) >= HintAfter {
			hinted = true
			onWait(NoTailnetHint)
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			if ip, ok := TailscaleIPv4(); ok {
				return ip, nil
			}
			return "", ErrNoTailscale
		case <-ticker.C:
			if ip, ok := TailscaleIPv4(); ok {
				return ip, nil
			}
			if onWait != nil {
				onWait("still waiting for Tailscale IPv4 in 100.64.0.0/10")
			}
			maybeHint()
		}
	}
}
