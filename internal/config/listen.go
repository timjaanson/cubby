package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// DefaultPort spells CUBBY on a phone keypad. It sits outside the ports dev
// tools usually take (3000, 5173, 8000, 8080, 9000) and below the Linux
// ephemeral range, so a fresh install rarely collides with anything.
const DefaultPort = 28229

// DefaultListen is the address serve binds when nothing else is set.
var DefaultListen = ":" + strconv.Itoa(DefaultPort)

// ListenAddr turns what a user typed into an address to bind. A bare port
// ("9000" or ":9000") binds every interface; "127.0.0.1:9000" and
// "[::1]:9000" bind one.
func ListenAddr(s string) (string, error) {
	s = strings.TrimSpace(s)
	if isPort(s) {
		s = ":" + s
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil || !isPort(port) {
		return "", fmt.Errorf("listen address %q should be a port like 9000 or host:port like 127.0.0.1:9000", s)
	}
	return net.JoinHostPort(host, port), nil
}

// ServerURL turns what a user typed into the base URL of a server. A bare
// port ("9000" or ":9000") means this machine; a host without a scheme gets
// http://.
func ServerURL(s string) string {
	s = strings.TrimSpace(s)
	if p := strings.TrimPrefix(s, ":"); isPort(p) {
		return "http://localhost:" + p
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	return strings.TrimRight(s, "/")
}

func isPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535 && s == strconv.Itoa(n)
}
