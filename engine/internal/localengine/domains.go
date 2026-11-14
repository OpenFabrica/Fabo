//go:build linux || darwin

package localengine

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type domainPorts struct {
	HTTP  uint16 `json:"http"`
	HTTPS uint16 `json:"https"`
}

var domainDefaults = [2]uint16{80, 443}
var domainEnv = [2]string{"FABO_HTTP_PORT", "FABO_HTTPS_PORT"}

type domainListener struct {
	port      uint16
	listeners []net.Listener
}

func (l *domainListener) release() {
	for _, socket := range l.listeners {
		_ = socket.Close()
	}
	l.listeners = nil
}

// Domains manages the daemon's container-domain listeners independently of the
// VM, so the configured ports survive guest shutdown and restart.
type Domains struct {
	mu      sync.Mutex
	dir     string
	tlsDir  string
	ports   [2]uint16
	env     [2]uint16
	routes  [2]domainListener
	servers [2]*http.Server
	tls     *tls.Config
	cancel  context.CancelFunc
	done    chan struct{}
	dns     net.PacketConn
}

func OpenDomains(ctx context.Context, dir, tlsDir string, defaults bool, dial func(context.Context, string) (net.Conn, error)) (*Domains, error) {
	if tlsDir == "" {
		tlsDir = filepath.Join(dir, "tls")
	}
	d := &Domains{dir: dir, tlsDir: tlsDir, ports: domainDefaults, done: make(chan struct{})}
	raw, err := os.ReadFile(filepath.Join(dir, "domains.json"))
	saved := err == nil
	if saved {
		var ports domainPorts
		if err := json.Unmarshal(raw, &ports); err != nil {
			return nil, fmt.Errorf("read domain ports: %w", err)
		}
		if ports.HTTP != 0 {
			d.ports[0] = ports.HTTP
		}
		if ports.HTTPS != 0 {
			d.ports[1] = ports.HTTPS
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for i, name := range domainEnv {
		port, _ := strconv.ParseUint(strings.TrimSpace(os.Getenv(name)), 10, 16)
		d.env[i] = uint16(port)
		if port != 0 {
			d.ports[i] = uint16(port)
		}
	}
	ctx, d.cancel = context.WithCancel(ctx)
	transport := &http.Transport{
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 30 * time.Second,
		DialContext:           func(ctx context.Context, _, address string) (net.Conn, error) { return dial(ctx, address) },
	}
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.URL.Scheme, p.Out.URL.Host = "http", p.In.Host
			p.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, errMissing) {
				http.Error(w, "Container domain not found", http.StatusNotFound)
			} else {
				http.Error(w, "Container HTTP service unavailable", http.StatusBadGateway)
			}
		},
	}
	for i := range d.servers {
		d.servers[i] = &http.Server{
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    64 << 10,
			BaseContext:       func(net.Listener) context.Context { return ctx },
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, name := domainHost(r.Host); name == "" {
					http.Error(w, "Container domain not found", http.StatusNotFound)
					return
				}
				proxy.ServeHTTP(w, r)
			}),
		}
		// Match the standard Linux service's HTTP default. HTTPS is
		// enabled there when its ports are explicitly configured.
		standard := defaults && (i == 0 || runtime.GOOS == "darwin")
		if standard || saved || d.env[i] != 0 || runtime.GOOS == "darwin" && (d.env[0] != 0 || d.env[1] != 0) {
			if err := d.bind(&d.routes[i], i, d.ports[i]); err != nil {
				log.Printf("container domain router: %v", err)
			} else {
				d.serve(i)
			}
		}
	}
	if defaults && runtime.GOOS == "darwin" {
		socket, err := net.ListenPacket("udp4", "127.0.0.1:53535")
		if err != nil {
			log.Printf("container domain DNS: %v", err)
		} else {
			d.dns = socket
			go serveDomainDNS(socket)
		}
	}
	go d.reclaim(ctx)
	return d, nil
}

func (d *Domains) bind(route *domainListener, kind int, port uint16) error {
	if kind == 1 && d.tls == nil {
		config, err := domainTLS(d.tlsDir)
		if err != nil {
			return err
		}
		d.tls = config
	}
	socket, err := listenDomain("tcp4", "127.0.0.1", "0.0.0.0", port)
	if err != nil {
		return err
	}
	v6, err := listenDomain("tcp6", "::1", "::", port)
	if err != nil && !errors.Is(err, syscall.EAFNOSUPPORT) && !errors.Is(err, syscall.EADDRNOTAVAIL) && !errors.Is(err, syscall.EPROTONOSUPPORT) {
		_ = socket.Close()
		return err
	}
	route.port, route.listeners = port, []net.Listener{socket}
	if v6 != nil {
		route.listeners = append(route.listeners, v6)
	}
	return nil
}

// On macOS an unprivileged process can bind a low port on the wildcard
// address. Keep the same loopback-only admission as the Rust router.
func listenDomain(network, loopback, wildcard string, port uint16) (net.Listener, error) {
	service := strconv.Itoa(int(port))
	l, err := net.Listen(network, net.JoinHostPort(loopback, service))
	if runtime.GOOS != "darwin" || !errors.Is(err, syscall.EACCES) {
		return l, err
	}
	l, err = net.Listen(network, net.JoinHostPort(wildcard, service))
	if err != nil {
		return nil, err
	}
	return localDomainListener{l}, nil
}

type localDomainListener struct{ net.Listener }

func (l localDomainListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if conn.RemoteAddr().(*net.TCPAddr).IP.IsLoopback() {
			return conn, nil
		}
		_ = conn.Close()
	}
}

func (d *Domains) serve(kind int) {
	for _, socket := range d.routes[kind].listeners {
		if kind == 1 {
			socket = tls.NewListener(socket, d.tls)
		}
		go func() {
			if err := d.servers[kind].Serve(socket); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("container domain router: %v", err)
			}
		}()
	}
}

func (d *Domains) reclaim(ctx context.Context) {
	defer close(d.done)
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			d.mu.Lock()
			for i := range d.routes {
				if r := &d.routes[i]; r.port != 0 && len(r.listeners) == 0 && d.bind(r, i, r.port) == nil {
					d.serve(i)
				}
			}
			d.mu.Unlock()
		}
	}
}

// Yield releases a TCP router while a container claims its host port.
func (d *Domains) Yield(port uint16) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.routes {
		r := &d.routes[i]
		if r.port == port && len(r.listeners) != 0 {
			r.release()
			return true
		}
	}
	return false
}

func (d *Domains) Close() {
	if d.dns != nil {
		_ = d.dns.Close()
	}
	d.cancel()
	<-d.done
	for _, srv := range d.servers {
		_ = srv.Close()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.routes {
		d.routes[i].release()
	}
}

type domainStatus struct {
	Port    uint16  `json:"port"`
	Default uint16  `json:"default"`
	Running bool    `json:"running"`
	Serving bool    `json:"serving"`
	Env     *string `json:"env"`
}

func (d *Domains) report() map[string]domainStatus {
	report := make(map[string]domainStatus, 2)
	for i, key := range []string{"http", "https"} {
		status := domainStatus{Port: d.ports[i], Default: domainDefaults[i], Running: d.routes[i].port != 0, Serving: len(d.routes[i].listeners) != 0}
		if d.env[i] != 0 {
			status.Env = &domainEnv[i]
		}
		report[key] = status
	}
	return report
}

func (ports domainPorts) validate() error {
	if ports.HTTP == 0 || ports.HTTPS == 0 || ports.HTTP == ports.HTTPS {
		return fmt.Errorf("%w: HTTP and HTTPS require distinct ports from 1 to 65535", errInvalid)
	}
	return nil
}

func (d *Domains) set(ports domainPorts) error {
	if err := ports.validate(); err != nil {
		return err
	}
	want := [2]uint16{ports.HTTP, ports.HTTPS}
	for i, port := range want {
		if d.env[i] != 0 && d.env[i] != port {
			return fmt.Errorf("%w: %s sets this port to %d", errConflict, domainEnv[i], d.env[i])
		}
	}
	var staged [2]domainListener
	for i, port := range want {
		if port == d.routes[i].port {
			continue
		}
		if err := d.bind(&staged[i], i, port); err != nil {
			for j := range staged {
				staged[j].release()
			}
			if errors.Is(err, syscall.EADDRINUSE) {
				return fmt.Errorf("%w: port %d is in use", errConflict, port)
			}
			return err
		}
	}
	if err := saveJSON(d.dir, "domains.json", ports); err != nil {
		for i := range staged {
			staged[i].release()
		}
		return err
	}
	for i, next := range staged {
		if next.port != 0 {
			d.routes[i].release()
			d.routes[i] = next
			d.serve(i)
		}
	}
	d.ports = want
	return nil
}

func (d *Domains) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var ports domainPorts
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&ports); err != nil {
			writeError(w, fmt.Errorf("%w: invalid domain ports", errInvalid))
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if err := d.set(ports); err != nil {
			writeError(w, err)
			return
		}
	case http.MethodGet, http.MethodHead:
		d.mu.Lock()
		defer d.mu.Unlock()
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, d.report())
}
