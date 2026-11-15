// Package sharetunnel is the Fabo share client: a public HTTPS URL for a
// local port or container, through a fabo-edge relay.
//
// The daemon is its only user. It keeps the account in a Config file,
// talks to the edge's /v1 API with [API], and runs tunnels with [Start].
// The wire format lives in package sharewire; the behaviour follows
// upstream Kiri's crates/kiri/src/share.rs.
package sharetunnel

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// DefaultServer is the edge used when neither the config nor
// FABO_SHARE_SERVER names one, as `host[:port]`. Release builds set it with
// -ldflags "-X github.com/OpenFabrica/Fabo/engine/internal/sharetunnel.DefaultServer=…";
// it is empty in source so no relay is assumed.
var DefaultServer = ""

// ErrNoServer reports that no relay is configured.
var ErrNoServer = errors.New("no share relay is configured; run `fabo share login --server HOST`")

// ErrSignedOut reports a call that needs an account without one.
var ErrSignedOut = errors.New("not signed in; run `fabo share login`")

// Config is the account file, `<state root>/share.json`.
type Config struct {
	// Server is the relay as `host[:port]`; empty means DefaultServer.
	Server string `json:"server,omitempty"`
	// Token was issued by TokenServer and is only ever sent there.
	Token string `json:"token,omitempty"`
	// TokenServer is the normalized relay that issued Token.
	TokenServer string `json:"tokenServer,omitempty"`
	Login       string `json:"login,omitempty"`
	// Device is the random id of this install.
	Device string `json:"device"`
}

// LoadConfig reads path, creating the device id (and the file) on first use.
func LoadConfig(path string) (Config, error) {
	var cfg Config
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return cfg, err
	default:
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("%s: %w", path, err)
		}
	}
	if cfg.Device == "" {
		cfg.Device = randomHex(16)
		if err := SaveConfig(path, cfg); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

// SaveConfig writes cfg atomically with mode 0600, since it holds a token.
func SaveConfig(path string, cfg Config) error {
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// EffectiveServer returns the relay to use: FABO_SHARE_SERVER, then the
// config, then DefaultServer; empty when none is set.
func (c Config) EffectiveServer() string {
	if v := strings.TrimSpace(os.Getenv("FABO_SHARE_SERVER")); v != "" {
		return v
	}
	if c.Server != "" {
		return c.Server
	}
	return DefaultServer
}

// EffectiveToken returns FABO_SHARE_TOKEN when set (for CI), else the saved
// token when the relay in effect is the one that issued it. A server chosen
// by FABO_SHARE_SERVER or a later config change never receives another
// relay's token: the account then reads as signed out.
func (c Config) EffectiveToken() string {
	if v := strings.TrimSpace(os.Getenv("FABO_SHARE_TOKEN")); v != "" {
		return v
	}
	if c.Token == "" || c.TokenServer == "" || NormalizeServer(c.TokenServer) != NormalizeServer(c.EffectiveServer()) {
		return ""
	}
	return c.Token
}

// SetToken records token as issued by the relay in effect now.
func (c *Config) SetToken(token, login string) {
	c.Token, c.Login, c.TokenServer = token, login, ""
	if token != "" {
		c.TokenServer = NormalizeServer(c.EffectiveServer())
	}
}

// Server is a parsed relay address.
type Server struct {
	// Host is the TLS server name and the API's Host header.
	Host string
	Port int
	// Connect, when set, is dialed instead of Host:Port (FABO_SHARE_CONNECT),
	// keeping Host for TLS.
	Connect string
	// Roots trusts the system roots plus FABO_SHARE_CA.
	Roots *x509.CertPool
}

// NormalizeServer cleans a user-typed relay address into `host[:port]`.
func NormalizeServer(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "https://")
	return strings.TrimRight(s, "/")
}

// ParseServer turns `host[:port]` into a Server, reading FABO_SHARE_CONNECT
// and FABO_SHARE_CA.
func ParseServer(s string) (Server, error) {
	s = NormalizeServer(s)
	if s == "" {
		return Server{}, ErrNoServer
	}
	srv := Server{Host: s, Port: 443}
	if host, port, err := net.SplitHostPort(s); err == nil {
		n, err := strconv.Atoi(port)
		if err != nil || n <= 0 || n > 65535 {
			return Server{}, fmt.Errorf("share relay %q has a bad port", s)
		}
		srv.Host, srv.Port = host, n
	}
	if srv.Host == "" {
		return Server{}, fmt.Errorf("share relay %q has no host", s)
	}
	srv.Connect = strings.TrimSpace(os.Getenv("FABO_SHARE_CONNECT"))
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if ca := os.Getenv("FABO_SHARE_CA"); ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			return Server{}, fmt.Errorf("FABO_SHARE_CA: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return Server{}, fmt.Errorf("FABO_SHARE_CA: %s holds no PEM certificates", ca)
		}
	}
	srv.Roots = roots
	return srv, nil
}

// String returns the address as people type it.
func (s Server) String() string {
	if s.Port == 443 {
		return s.Host
	}
	return net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
}

// dialAddr is where TCP connects.
func (s Server) dialAddr() string {
	if s.Connect != "" {
		return s.Connect
	}
	return net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
}

// tlsConfig offers alpn and verifies the relay as Host.
func (s Server) tlsConfig(alpn string) *tls.Config {
	return &tls.Config{ServerName: s.Host, RootCAs: s.Roots, NextProtos: []string{alpn}, MinVersion: tls.VersionTLS12}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var (
	machineOnce sync.Once
	machineRaw  string
	hostOnce    sync.Once
	hostValue   string
)

// MachineID is a stable hash of this computer's hardware id, the same for
// every Fabo flavor on it; device when the id can't be read.
func MachineID(device string) string {
	machineOnce.Do(func() {
		switch runtime.GOOS {
		case "darwin":
			out, err := exec.Command("/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
			if err == nil {
				machineRaw = platformUUID(string(out))
			}
		default:
			raw, err := os.ReadFile("/etc/machine-id")
			if err == nil {
				machineRaw = strings.TrimSpace(string(raw))
			}
		}
	})
	if machineRaw == "" {
		return device
	}
	sum := sha256.Sum256([]byte("fabo-share-machine:" + machineRaw))
	return hex.EncodeToString(sum[:12])
}

// platformUUID picks IOPlatformUUID out of ioreg output.
func platformUUID(ioreg string) string {
	for _, line := range strings.Split(ioreg, "\n") {
		if !strings.Contains(line, `"IOPlatformUUID"`) {
			continue
		}
		_, v, ok := strings.Cut(line, "=")
		if !ok {
			return ""
		}
		return strings.Trim(strings.TrimSpace(v), `"`)
	}
	return ""
}

// HostName is this computer's name as people know it: the Mac's Computer
// Name, else the hostname.
func HostName() string {
	hostOnce.Do(func() {
		var name string
		if runtime.GOOS == "darwin" {
			if out, err := exec.Command("/usr/sbin/scutil", "--get", "ComputerName").Output(); err == nil {
				name = string(out)
			}
		}
		if strings.TrimSpace(name) == "" {
			name, _ = os.Hostname()
		}
		hostValue = cleanHost(name)
	})
	return hostValue
}

// cleanHost keeps one line of at most 64 characters without control characters.
func cleanHost(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsControl(r) {
			continue
		}
		if n == 64 {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
