package sharetunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrSessionExpired reports a token the relay no longer accepts.
var ErrSessionExpired = errors.New("your sign-in is no longer valid; run `fabo share login`")

// ErrUnsupported reports an endpoint the relay doesn't offer.
var ErrUnsupported = errors.New("the share relay doesn't support this")

// API calls a relay's /v1 endpoints on the same host and port as tunnels.
type API struct {
	Server Server
	// Token signs requests that need an account; empty for sign-in calls.
	Token string
	// UserAgent identifies the client, such as `fabo/0.3.0`.
	UserAgent string
	client    *http.Client
}

// NewAPI returns an API for srv, speaking HTTP/1.1 over TLS.
func NewAPI(srv Server, token, userAgent string) *API {
	transport := &http.Transport{
		TLSClientConfig:     srv.tlsConfig("http/1.1"),
		ForceAttemptHTTP2:   false,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   true,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 10 * time.Second}
			return d.DialContext(ctx, network, srv.dialAddr())
		},
	}
	return &API{Server: srv, Token: token, UserAgent: userAgent, client: &http.Client{Transport: transport, Timeout: 30 * time.Second}}
}

// call sends one JSON request and decodes the reply into out (when non-nil
// and the reply is JSON); it returns the status code.
func (a *API) call(ctx context.Context, method, path string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(raw)
	}
	u := url.URL{Scheme: "https", Host: a.Server.String(), Path: path}
	if before, query, ok := strings.Cut(path, "?"); ok {
		u.Path, u.RawQuery = before, query
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if a.UserAgent != "" {
		req.Header.Set("User-Agent", a.UserAgent)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("can't reach the share relay %s: %w", a.Server, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return resp.StatusCode, &StatusError{Status: resp.StatusCode, Message: e.Error}
		}
		return resp.StatusCode, &StatusError{Status: resp.StatusCode, Message: fmt.Sprintf("the share relay answered %d", resp.StatusCode)}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("the share relay sent a bad reply: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// signed is call for endpoints that need an account.
func (a *API) signed(ctx context.Context, method, path string, body, out any) (int, error) {
	if a.Token == "" {
		return 0, ErrSignedOut
	}
	status, err := a.call(ctx, method, path, body, out)
	if status == http.StatusUnauthorized {
		return status, ErrSessionExpired
	}
	return status, err
}

// StatusError is an error reply from the relay.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string { return e.Message }

// Login is a signed-in account.
type Login struct {
	Token string `json:"token"`
	Login string `json:"login"`
}

// LoginDev signs in without GitHub; only relays started with -dev allow it.
func (a *API) LoginDev(ctx context.Context, login, device string) (Login, error) {
	var out Login
	if _, err := a.call(ctx, "POST", "/v1/login/dev", map[string]string{"login": login, "device": device}, &out); err != nil {
		return Login{}, err
	}
	if out.Token == "" || out.Login == "" {
		return Login{}, errors.New("the share relay sent no token")
	}
	return out, nil
}

// DeviceCode is step one of the GitHub device flow.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}

// maxDeviceCodeSecs bounds the relay's untrusted interval and expiry.
const maxDeviceCodeSecs = 3600

// PollInterval is the wait between polls, clamped to 1s..1h.
func (d DeviceCode) PollInterval() time.Duration {
	n := d.Interval
	if n <= 0 {
		n = 5
	}
	return time.Duration(min(n, maxDeviceCodeSecs)) * time.Second
}

// Lifetime is how long the code may be polled, at most an hour.
func (d DeviceCode) Lifetime() time.Duration {
	n := d.ExpiresIn
	if n <= 0 {
		n = 900
	}
	return time.Duration(min(n, maxDeviceCodeSecs)) * time.Second
}

// LoginStart begins the GitHub device flow.
func (a *API) LoginStart(ctx context.Context) (DeviceCode, error) {
	var out DeviceCode
	if _, err := a.call(ctx, "POST", "/v1/login/start", map[string]string{}, &out); err != nil {
		return DeviceCode{}, err
	}
	if out.DeviceCode == "" || out.UserCode == "" || out.VerificationURI == "" {
		return DeviceCode{}, errors.New("the share relay sent an incomplete sign-in code")
	}
	return out, nil
}

// LoginPoll asks whether the code was approved: pending is true (with an
// extra wait when the relay asks to slow down) until it was.
func (a *API) LoginPoll(ctx context.Context, code, device string) (login Login, pending bool, slowDown bool, err error) {
	var out struct {
		Token    string `json:"token"`
		Login    string `json:"login"`
		SlowDown bool   `json:"slow_down"`
	}
	status, err := a.call(ctx, "POST", "/v1/login/poll", map[string]string{"device_code": code, "device": device}, &out)
	if err != nil {
		return Login{}, false, false, err
	}
	if status == http.StatusAccepted {
		return Login{}, true, out.SlowDown, nil
	}
	if out.Token == "" || out.Login == "" {
		return Login{}, false, false, errors.New("the share relay sent no token")
	}
	return Login{Token: out.Token, Login: out.Login}, false, false, nil
}

// Plan describes the account's effective limits and optional paid/gift expiry.
type Plan struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Tunnels int        `json:"tunnels"`
	Names   int        `json:"names"`
	Traffic uint64     `json:"traffic"`
	Expires *time.Time `json:"expires"`
}

// Account is GET /v1/whoami, with billing fields absent on legacy relays.
type Account struct {
	Billing bool   `json:"billing,omitempty"`
	Plan    Plan   `json:"plan,omitzero"`
	Login   string `json:"login"`
	Trust   int    `json:"trust"`
	Limits  struct {
		Tunnels *uint64 `json:"tunnels"`
		Rate    uint64  `json:"rate"`
		Warning bool    `json:"warning"`
	} `json:"limits"`
	Usage struct {
		Used   uint64    `json:"used"`
		Cap    *uint64   `json:"cap,omitempty"`
		Resets time.Time `json:"resets,omitzero"`
	} `json:"usage"`
}

// Whoami returns the signed-in account and its limits.
func (a *API) Whoami(ctx context.Context) (Account, error) {
	var out Account
	_, err := a.signed(ctx, "GET", "/v1/whoami", nil, &out)
	return out, err
}

// BillingURL returns an account-bound single-use HTTPS URL for the relay's billing page.
func (a *API) BillingURL(ctx context.Context) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	_, err := a.signed(ctx, "POST", "/v1/billing/web", map[string]string{}, &out)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(out.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", errors.New("the share relay sent an invalid billing URL")
	}
	return out.URL, nil
}

// ReservedName is one of the account's names.
type ReservedName struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	InUse     bool   `json:"in_use"`
	Status    string `json:"status,omitempty"`
	CreatedAt string `json:"created_at"`
}

// Pending reports a name still waiting for review.
func (n ReservedName) Pending() bool { return n.Status == "pending" }

// Names is GET /v1/names.
type Names struct {
	Items   []ReservedName `json:"items"`
	Limit   *uint64        `json:"limit"`
	Refused []struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	} `json:"refused,omitempty"`
}

// Names lists the account's reserved names.
func (a *API) Names(ctx context.Context) (Names, error) {
	var out Names
	_, err := a.signed(ctx, "GET", "/v1/names", nil, &out)
	return out, err
}

// Available reports whether name can be reserved.
func (a *API) Available(ctx context.Context, name string) bool {
	var out struct {
		Available bool `json:"available"`
	}
	status, err := a.signed(ctx, "GET", "/v1/names/check?name="+url.QueryEscape(name), nil, &out)
	return err == nil && status == http.StatusOK && out.Available
}

// Reserve asks for name for this account.
func (a *API) Reserve(ctx context.Context, name string) (ReservedName, error) {
	var out ReservedName
	status, err := a.signed(ctx, "POST", "/v1/names", map[string]string{"name": name}, &out)
	var se *StatusError
	if status == http.StatusForbidden && errors.As(err, &se) && !strings.Contains(se.Message, "isn't open") {
		return out, fmt.Errorf("%s. Release one with `fabo share release NAME`, or see your names with `fabo share names`", strings.TrimRight(se.Message, "."))
	}
	return out, err
}

// Release gives a reserved name back.
func (a *API) Release(ctx context.Context, name string) error {
	status, err := a.signed(ctx, "DELETE", "/v1/names/"+url.PathEscape(strings.ToLower(name)), nil, nil)
	if status == http.StatusNotFound {
		return fmt.Errorf("`%s` isn't one of your reserved names", name)
	}
	return err
}

// LiveTunnel is one of the account's live tunnels, from any computer.
type LiveTunnel struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	URL     string `json:"url"`
	Machine string `json:"machine"`
	Device  string `json:"device"`
	Host    string `json:"host"`
	Kind    string `json:"kind"`
	Client  string `json:"client"`
	Target  string `json:"target"`
	Started int64  `json:"started"`
}

// LiveTunnels is GET /v1/tunnels/live.
type LiveTunnels struct {
	Items []LiveTunnel `json:"items"`
	Limit *uint64      `json:"limit"`
}

// Tunnels lists the account's live tunnels.
func (a *API) Tunnels(ctx context.Context) (LiveTunnels, error) {
	var out LiveTunnels
	status, err := a.signed(ctx, "GET", "/v1/tunnels/live", nil, &out)
	if status == http.StatusNotFound {
		return out, ErrUnsupported
	}
	for i := range out.Items {
		if out.Items[i].Label == "" {
			out.Items[i].Label = out.Items[i].Name
		}
	}
	return out, err
}

// CloseTunnel stops one of the account's tunnels wherever it runs; host
// tells its owner where it was stopped from.
func (a *API) CloseTunnel(ctx context.Context, name, host string) error {
	status, err := a.signed(ctx, "DELETE", "/v1/tunnels/"+url.PathEscape(name), map[string]string{"host": host}, nil)
	if status == http.StatusNotFound {
		return fmt.Errorf("%s isn't live any more", name)
	}
	return err
}

// Reservation is what AutoReserve did about a requested name.
type Reservation struct {
	// Kind is mine, waiting, reserved, requested, full, unavailable, or
	// failed when the relay couldn't be asked (Error says why).
	Kind  string `json:"kind"`
	URL   string `json:"url,omitempty"`
	Limit uint64 `json:"limit,omitempty"`
	Error string `json:"error,omitempty"`
}

// AutoReserve reserves name when it's free and a slot is left, so the
// tunnel gets exactly that address. It never fails the share: any error
// leaves the relay to pick the address.
func (a *API) AutoReserve(ctx context.Context, name string) Reservation {
	name = strings.ToLower(name)
	mine, err := a.Names(ctx)
	if err != nil {
		// A slow or broken link is not a taken name; say which it was.
		return Reservation{Kind: "failed", Error: err.Error()}
	}
	for _, n := range mine.Items {
		if n.Name == name {
			if n.Pending() {
				return Reservation{Kind: "waiting", URL: n.URL}
			}
			return Reservation{Kind: "mine", URL: n.URL}
		}
	}
	if mine.Limit != nil && uint64(len(mine.Items)) >= *mine.Limit {
		return Reservation{Kind: "full", Limit: *mine.Limit}
	}
	if !a.Available(ctx, name) {
		if ctx.Err() != nil {
			return Reservation{Kind: "failed", Error: ctx.Err().Error()}
		}
		return Reservation{Kind: "unavailable"}
	}
	r, err := a.Reserve(ctx, name)
	switch {
	case err != nil:
		return Reservation{Kind: "unavailable"}
	case r.Pending():
		return Reservation{Kind: "requested", URL: r.URL}
	default:
		return Reservation{Kind: "reserved", URL: r.URL}
	}
}

// UserAgent formats the User-Agent for version.
func UserAgent(version string) string { return "fabo/" + version }
