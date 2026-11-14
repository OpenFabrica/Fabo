//go:build linux || darwin

package localengine

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

// UpdateWindows accepts fabo-win's current system proxy and WSL relay map.
// The snapshot belongs to the live Windows process and is never persisted.
func (p *Proxy) UpdateWindows(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var update struct {
		effectiveProxy
		Relay map[uint16]uint16 `json:"relay"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&update); err != nil {
		writeError(w, fmt.Errorf("%w: invalid Windows proxy settings", errInvalid))
		return
	}
	var err error
	if update.HTTP, err = normalizeProxy(update.HTTP); err == nil {
		update.HTTPS, err = normalizeProxy(update.HTTPS)
	}
	if err != nil {
		writeError(w, fmt.Errorf("%w: invalid Windows proxy address", errInvalid))
		return
	}
	for original, local := range update.Relay {
		if original == 0 || local == 0 {
			writeError(w, fmt.Errorf("%w: invalid Windows proxy relay port", errInvalid))
			return
		}
	}
	if update.NoProxy == nil {
		update.NoProxy = []string{}
	}
	p.mu.Lock()
	p.windows = &update.effectiveProxy
	if update.Relay != nil {
		p.relay = update.Relay
	} else if p.relay == nil {
		p.relay = map[uint16]uint16{}
	}
	p.transport.CloseIdleConnections()
	p.mu.Unlock()
	p.report(w)
}

// ConnectWindows forwards a guest agent's Unix-socket CONNECT to the
// current relay port, restricted to the active Windows proxy addresses.
func (p *Proxy) ConnectWindows(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != http.MethodConnect {
		w.Header().Set("Allow", "CONNECT")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	port, err := strconv.ParseUint(strings.TrimPrefix(path, guestwire.WindowsProxyPath), 10, 16)
	config, active, relay := p.effective()
	activePort := func(raw string) uint16 {
		u, _ := url.Parse(raw)
		return windowsProxyPort(u)
	}
	local := relay[uint16(port)]
	if err != nil || !config.Containers || port == 0 || local == 0 || (uint16(port) != activePort(active.HTTP) && uint16(port) != activePort(active.HTTPS)) {
		http.Error(w, "Windows proxy relay unavailable", http.StatusBadGateway)
		return
	}
	upstream, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(r.Context(), "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(local))))
	if err != nil {
		http.Error(w, "Windows proxy relay unavailable", http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	stream, ok := conn.(guestwire.TCPStream)
	if !ok {
		return
	}
	if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil || rw.Flush() != nil {
		return
	}
	guestwire.BridgeTCP(proxyTunnel{TCPStream: stream, reader: rw.Reader}, upstream.(guestwire.TCPStream))
}

type proxyTunnel struct {
	guestwire.TCPStream
	reader *bufio.Reader
}

func (t proxyTunnel) Read(b []byte) (int, error) { return t.reader.Read(b) }
