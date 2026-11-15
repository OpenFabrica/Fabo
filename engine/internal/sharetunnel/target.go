package sharetunnel

import (
	"bytes"
	"net"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Target is what a tunnel forwards to.
type Target struct {
	// Addr is `host:port` for port targets; empty for containers, which the
	// daemon dials itself.
	Addr string
	// Label is the suggested first label of the public hostname (a
	// container or host name), or empty.
	Label string
	// Display describes the target to people: `localhost:3000`, `web`.
	Display string
}

// ParseAddr reads `3000` or `host:port`; ok is false for anything else,
// such as a container name.
func ParseAddr(arg string) (Target, bool) {
	if arg != "" && len(arg) <= 5 && allDigits(arg) {
		port, _ := strconv.Atoi(arg)
		if port <= 0 || port > 65535 {
			return Target{}, false
		}
		addr := "localhost:" + strconv.Itoa(port)
		return Target{Addr: addr, Display: addr}, true
	}
	i := strings.LastIndexByte(arg, ':')
	if i < 0 {
		return Target{}, false
	}
	host, portText := arg[:i], arg[i+1:]
	if !allDigits(portText) || len(portText) > 5 {
		return Target{}, false
	}
	port, _ := strconv.Atoi(portText)
	if port <= 0 || port > 65535 {
		return Target{}, false
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "" {
		return Target{}, false
	}
	label := ""
	local := host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0"
	if !local && net.ParseIP(host) == nil {
		label, _, _ = strings.Cut(host, ".")
	}
	addr := net.JoinHostPort(host, portText)
	return Target{Addr: addr, Label: label, Display: addr}, true
}

// IsShorthand reports whether `fabo ARG` means "share this": a port number
// or `host:port`. No Fabo or docker command looks like either.
func IsShorthand(arg string) bool {
	_, ok := ParseAddr(arg)
	return ok
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseRequestLine reads `GET /path HTTP/1.1` at the start of a stream.
func parseRequestLine(b []byte) (method, path string, ok bool) {
	if len(b) > 8192 {
		b = b[:8192]
	}
	end := bytes.IndexAny(b, "\r\n")
	if end < 0 || !utf8.Valid(b[:end]) {
		return "", "", false
	}
	parts := strings.Split(string(b[:end]), " ")
	if len(parts) != 3 {
		return "", "", false
	}
	method, path = parts[0], parts[1]
	if method == "" || len(method) > 16 || !strings.HasPrefix(parts[2], "HTTP/") {
		return "", "", false
	}
	for i := 0; i < len(method); i++ {
		if method[i] < 'A' || method[i] > 'Z' {
			return "", "", false
		}
	}
	return method, path, true
}

// parseStatusLine reads `HTTP/1.1 200 OK` at the start of a response.
func parseStatusLine(b []byte) (int, bool) {
	rest, ok := bytes.CutPrefix(b, []byte("HTTP/1."))
	if !ok || len(rest) < 1 {
		return 0, false
	}
	rest, ok = bytes.CutPrefix(rest[1:], []byte(" "))
	if !ok || len(rest) < 3 || !allDigits(string(rest[:3])) {
		return 0, false
	}
	if len(rest) > 3 && rest[3] != ' ' && rest[3] != '\r' && rest[3] != '\n' {
		return 0, false
	}
	code, _ := strconv.Atoi(string(rest[:3]))
	return code, true
}
