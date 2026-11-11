package listenaddr

import (
	"net"
	"strconv"
)

// DefaultPort is the string-can listen port.
const DefaultPort = 8741

// Host returns the Tailscale IPv4 address if one is present, otherwise 127.0.0.1.
func Host() string {
	_, tsNet, err := net.ParseCIDR("100.64.0.0/10")
	if err != nil {
		return "127.0.0.1"
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
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
			if ip4 != nil && tsNet.Contains(ip4) {
				return ip4.String()
			}
		}
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
