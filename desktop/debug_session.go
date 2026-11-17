package main

// Debug sessions: the saved side of the Debug page. A session is what the
// user configured (a client's target, a server's port, a mock's routes);
// the messages it exchanges live only in memory (see debug_net.go). The
// sessions are kept in debug-sessions.json beside desktop.json, so each
// installation keeps its own and the engine holds none of it.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// Session kinds and protocols.
const (
	debugClient = "client"
	debugServer = "server"

	debugTCP  = "tcp"
	debugUDP  = "udp"
	debugWS   = "ws"
	debugHTTP = "http"
)

// debugStoreVersion is written into debug-sessions.json; a file from a
// newer version is still read, field by field.
const debugStoreVersion = 1

// debugHeader is one request header of an HTTP client session.
type debugHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// debugRoute is one route of an HTTP mock: requests matching Method and
// Path get Status and Body after Delay milliseconds, or the files under
// Dir when it is set.
type debugRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Status      int    `json:"status"`
	ContentType string `json:"contentType,omitempty"`
	Body        string `json:"body,omitempty"`
	Delay       int    `json:"delay,omitempty"`
	Dir         string `json:"dir,omitempty"`
}

// debugSession is one saved session of the Debug page.
type debugSession struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Proto string `json:"proto"`
	Name  string `json:"name"`
	Note  string `json:"note,omitempty"`

	// A client's target: Address is host:port, or a URL for WebSocket and
	// HTTP. Container and Port name a container's port instead; Via says
	// how to reach it, "host" through its published port or "net" through
	// the engine (see debugDialContainer).
	Address   string `json:"address,omitempty"`
	Container string `json:"container,omitempty"`
	Port      int    `json:"port,omitempty"`
	Via       string `json:"via,omitempty"`

	// A server's port and where it listens: loopback, or every interface
	// when BindAll. AutoListen starts it with the window.
	BindAll    bool   `json:"bindAll,omitempty"`
	AutoListen bool   `json:"autoListen,omitempty"`
	Greeting   string `json:"greeting,omitempty"`
	AutoReply  bool   `json:"autoReply,omitempty"`
	Reply      string `json:"reply,omitempty"`
	TLS        bool   `json:"tls,omitempty"`
	// Binds are more addresses a loopback server listens on, set at start
	// and not saved: on Linux the bridges' gateways, which containers reach
	// it by.
	Binds []string `json:"-"`
	// Insecure skips checking a WebSocket or HTTP server's certificate.
	Insecure bool   `json:"insecure,omitempty"`
	CertFile string `json:"certFile,omitempty"`
	KeyFile  string `json:"keyFile,omitempty"`

	// How messages are typed and shown.
	Encoding string   `json:"encoding,omitempty"`
	NoEscape bool     `json:"noEscape,omitempty"`
	Quick    []string `json:"quick,omitempty"`

	// An HTTP client's request.
	Method  string        `json:"method,omitempty"`
	Headers []debugHeader `json:"headers,omitempty"`
	Body    string        `json:"body,omitempty"`

	// An HTTP mock's routes.
	Routes []debugRoute `json:"routes,omitempty"`
}

// debugStore is the content of debug-sessions.json.
type debugStore struct {
	Version  int            `json:"version"`
	Sessions []debugSession `json:"sessions"`
}

// debugSessionsPath places the sessions beside the window's settings.
func debugSessionsPath(prefsFile string) string {
	if prefsFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(prefsFile), "debug-sessions.json")
}

// loadDebugSessions reads the saved sessions; a missing or unreadable file
// is an empty list, so the page always opens.
func loadDebugSessions(path string) []debugSession {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var store debugStore
	if json.Unmarshal(data, &store) != nil {
		return nil
	}
	out := store.Sessions[:0]
	for _, x := range store.Sessions {
		if x.ID != "" && x.valid() == nil {
			out = append(out, x)
		}
	}
	return out
}

// saveDebugSessions writes the sessions through a temporary file and a
// rename, so a crash never leaves half a file. Two windows sharing the
// file (a development build beside the installed app) keep the last write.
func saveDebugSessions(path string, sessions []debugSession) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(debugStore{Version: debugStoreVersion, Sessions: sessions}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".debug-sessions-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o600)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// valid reports what is wrong with a session's settings, if anything.
func (x debugSession) valid() error {
	switch x.Kind {
	case debugClient, debugServer:
	default:
		return fmt.Errorf("unknown kind %q", x.Kind)
	}
	switch x.Proto {
	case debugTCP, debugUDP, debugWS, debugHTTP:
	default:
		return fmt.Errorf("unknown protocol %q", x.Proto)
	}
	if x.Kind == debugServer {
		if x.Port < 1 || x.Port > 65535 {
			return errors.New(tr("端口要是 1–65535 的数字", "The port must be a number from 1 to 65535"))
		}
		return nil
	}
	if x.Container != "" {
		if x.Port < 1 || x.Port > 65535 {
			return errors.New(tr("请选择容器端口", "Choose a container port"))
		}
		return nil
	}
	if strings.TrimSpace(x.Address) == "" {
		return errors.New(tr("请填写地址", "Enter an address"))
	}
	return nil
}

// protoName is a protocol as the page shows it.
func protoName(proto string) string {
	switch proto {
	case debugWS:
		return "WebSocket"
	case debugHTTP:
		return "HTTP"
	}
	return strings.ToUpper(proto)
}

// badge is the short protocol mark of a session's row.
func (x debugSession) badge() string {
	if x.Proto == debugWS {
		return "WS"
	}
	return strings.ToUpper(x.Proto)
}

// listenAddress is where a server binds, as its head shows it: loopback
// unless it was opened to every interface. Containers reach a loopback
// server on a Mac, as the engine relays host.docker.internal to the
// loopback; on Linux it also listens on the Binds.
func (x debugSession) listenAddress() string {
	host := "127.0.0.1"
	if x.BindAll {
		host = "0.0.0.0"
	}
	return fmt.Sprintf("%s:%d", host, x.Port)
}

// listenAddresses are all the addresses a server binds.
func (x debugSession) listenAddresses() []string {
	out := []string{x.listenAddress()}
	if x.BindAll {
		return out
	}
	for _, host := range x.Binds {
		out = append(out, net.JoinHostPort(host, fmt.Sprint(x.Port)))
	}
	return out
}

// debugEncodings are the text encodings messages can be typed and shown in.
var debugEncodings = []string{"UTF-8", "GBK", "GB18030", "Big5", "Shift_JIS", "ISO-8859-1"}

// textEncoding returns the codec of a name; nil means UTF-8.
func textEncoding(name string) encoding.Encoding {
	switch name {
	case "GBK":
		return simplifiedchinese.GBK
	case "GB18030":
		return simplifiedchinese.GB18030
	case "Big5":
		return traditionalchinese.Big5
	case "Shift_JIS":
		return japanese.ShiftJIS
	case "ISO-8859-1":
		return charmap.ISO8859_1
	}
	return nil
}

// encodeText turns typed text into the bytes to send: escapes resolved
// unless raw, then encoded.
func encodeText(text, enc string, raw bool) ([]byte, error) {
	if !raw {
		var err error
		if text, err = unescape(text); err != nil {
			return nil, err
		}
	}
	codec := textEncoding(enc)
	if codec == nil {
		return []byte(text), nil
	}
	out, err := codec.NewEncoder().Bytes([]byte(text))
	if err != nil {
		return nil, fmt.Errorf(tr("文本无法用 %s 编码", "The text cannot be encoded as %s"), enc)
	}
	return out, nil
}

// decodeText shows received bytes as text in the session's encoding. Bytes
// that are not valid text in it keep their escaped form, so nothing is
// silently lost.
func decodeText(data []byte, enc string) string {
	if codec := textEncoding(enc); codec != nil {
		if out, err := codec.NewDecoder().Bytes(data); err == nil {
			data = out
		}
	}
	var b strings.Builder
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&b, `\x%02x`, data[0])
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
		data = data[size:]
	}
	return b.String()
}

// unescape resolves \r \n \t \0 \\ and \xNN, the escapes a single-line
// field needs to send control bytes. Anything else after a backslash is an
// error rather than a guess.
func unescape(s string) (string, error) {
	if !strings.Contains(s, `\`) {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 >= len(s) {
			return "", errors.New(tr("结尾的 \\ 后面缺少字符", "A trailing \\ escapes nothing"))
		}
		i++
		switch s[i] {
		case 'r':
			b.WriteByte('\r')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case '0':
			b.WriteByte(0)
		case '\\':
			b.WriteByte('\\')
		case 'x':
			if i+2 >= len(s) {
				return "", errors.New(tr("\\x 后面要有两位 16 进制数", "\\x needs two hex digits"))
			}
			v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return "", errors.New(tr("\\x 后面要有两位 16 进制数", "\\x needs two hex digits"))
			}
			b.WriteByte(byte(v))
			i += 2
		default:
			return "", fmt.Errorf(tr("不认识的转义 \\%c", "Unknown escape \\%c"), s[i])
		}
	}
	return b.String(), nil
}

// parseHex reads hex bytes typed with or without spaces, colons or 0x.
func parseHex(s string) ([]byte, error) {
	clean := strings.NewReplacer(" ", "", "\n", "", "\r", "", "\t", "", ":", "", "0x", "", "0X", "", ",", "").Replace(s)
	if clean == "" {
		return nil, errors.New(tr("请输入要发送的 16 进制", "Enter the hex to send"))
	}
	out, err := hex.DecodeString(clean)
	if err != nil {
		return nil, errors.New(tr("16 进制输入有误", "The hex is not valid"))
	}
	return out, nil
}

// hexDump shows bytes as hex pairs beside their printable characters.
func hexDump(data []byte) (pairs, text string) {
	var h, a bytes.Buffer
	for i, c := range data {
		if i > 0 {
			h.WriteByte(' ')
		}
		fmt.Fprintf(&h, "%02x", c)
		if c >= 0x20 && c < 0x7f {
			a.WriteByte(c)
		} else {
			a.WriteByte('.')
		}
	}
	return h.String(), a.String()
}
