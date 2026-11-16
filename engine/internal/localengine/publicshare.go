//go:build linux || darwin

package localengine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/OpenFabrica/Fabo/engine/sharetunnel"
)

// recentRequests is how many visitor requests each share keeps for the window.
const recentRequests = 50

// PublicShareOptions wires public share links to the daemon.
type PublicShareOptions struct {
	// ConfigPath is the account file, `<state root>/share.json`.
	ConfigPath string
	// Version goes into the client name the relay shows.
	Version string
	// ResolveContainer returns the canonical name of a running container,
	// waking the VM when needed.
	ResolveContainer func(ctx context.Context, name string) (string, error)
	// DialContainer connects to a container's HTTP service, waking the VM
	// when needed.
	DialContainer func(ctx context.Context, name string) (net.Conn, error)
}

// PublicShares hosts `fabo share` tunnels for the CLI and the window. It
// lives in the supervisor, beside the domain router, so links survive VM
// sleep; tunnels are not restored after a daemon restart.
type PublicShares struct {
	opts PublicShareOptions

	mu     sync.Mutex
	cfg    sharetunnel.Config
	shares []*publicShare
	// login is the one device-flow sign-in in progress; loginGen makes a
	// superseded poll's result harmless.
	login         *pendingLogin
	loginGen      uint64
	closed        bool
	billingCancel context.CancelFunc
	billingWG     sync.WaitGroup
	lastPlan      *sharetunnel.Plan
}

// pendingLogin is a GitHub device-flow sign-in waiting for approval.
type pendingLogin struct {
	gen    uint64
	cancel context.CancelFunc
	Code   string `json:"code"`
	URI    string `json:"uri"`
	Error  string `json:"error,omitempty"`
}

// publicShare is one tunnel and what it has served.
type publicShare struct {
	ID          string                   `json:"id"`
	Target      string                   `json:"target"`
	Name        string                   `json:"name,omitempty"`
	Owner       string                   `json:"owner"`
	URL         string                   `json:"url,omitempty"`
	State       string                   `json:"state"`
	Reason      string                   `json:"reason,omitempty"`
	Error       bool                     `json:"error,omitempty"`
	Started     time.Time                `json:"started"`
	Requests    uint64                   `json:"requests"`
	In          uint64                   `json:"in"`
	Out         uint64                   `json:"out"`
	Recent      []sharetunnel.Request    `json:"recent"`
	Reservation *sharetunnel.Reservation `json:"reservation,omitempty"`

	handle *sharetunnel.Handle
	subs   map[chan sharetunnel.Event]struct{}
	// ended is closed when the tunnel has ended for good.
	ended chan struct{}
}

// OpenPublicShares loads the account file (creating its device id).
func OpenPublicShares(opts PublicShareOptions) (*PublicShares, error) {
	cfg, err := sharetunnel.LoadConfig(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	return &PublicShares{opts: opts, cfg: cfg}, nil
}

// Close stops every tunnel and any sign-in in progress.
func (p *PublicShares) Close() {
	p.mu.Lock()
	p.closed = true
	p.cancelBillingLocked()
	handles := p.detachAllLocked()
	p.cancelLoginLocked()
	p.mu.Unlock()
	stopAll(handles)
	p.billingWG.Wait()
}

// detachAllLocked forgets every share and returns their handles to stop
// outside the lock (stopping waits for tunnel goroutines, whose events
// take the lock).
func (p *PublicShares) detachAllLocked() []*sharetunnel.Handle {
	var handles []*sharetunnel.Handle
	for _, s := range p.shares {
		if s.handle != nil {
			handles = append(handles, s.handle)
		}
	}
	p.shares = nil
	return handles
}

func stopAll(handles []*sharetunnel.Handle) {
	var wg sync.WaitGroup
	for _, h := range handles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Stop()
		}()
	}
	wg.Wait()
}

func (p *PublicShares) cancelLoginLocked() {
	p.loginGen++
	if p.login != nil && p.login.cancel != nil {
		p.login.cancel()
	}
	p.login = nil
}

// server returns the effective relay, or ErrNoServer.
func (p *PublicShares) serverLocked() (sharetunnel.Server, error) {
	return sharetunnel.ParseServer(p.cfg.EffectiveServer())
}

// apiLocked returns an API client for the relay with the saved token.
func (p *PublicShares) apiLocked() (*sharetunnel.API, error) {
	srv, err := p.serverLocked()
	if err != nil {
		return nil, err
	}
	return sharetunnel.NewAPI(srv, p.cfg.EffectiveToken(), sharetunnel.UserAgent(p.opts.Version)), nil
}

// accountView is the account part of GET /fabo/share.
type accountView struct {
	SignedIn bool          `json:"signedIn"`
	Login    string        `json:"login,omitempty"`
	Server   string        `json:"server"`
	Pending  *pendingLogin `json:"pending,omitempty"`
}

func (p *PublicShares) accountLocked() accountView {
	view := accountView{
		SignedIn: p.cfg.EffectiveToken() != "",
		Server:   sharetunnel.NormalizeServer(p.cfg.EffectiveServer()),
	}
	if view.SignedIn {
		view.Login = p.cfg.Login
	}
	// A copy: the view is encoded after the lock is released, while a
	// sign-in poll may still write the pending error.
	if p.login != nil {
		pending := *p.login
		pending.cancel = nil
		view.Pending = &pending
	}
	return view
}

// report is GET /fabo/share.
func (p *PublicShares) report() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	shares := make([]publicShare, 0, len(p.shares))
	for _, s := range p.shares {
		copy := *s
		copy.Recent = append([]sharetunnel.Request(nil), s.Recent...)
		shares = append(shares, copy)
	}
	return map[string]any{"account": p.accountLocked(), "shares": shares}
}

func (p *PublicShares) find(id string) *publicShare {
	for _, s := range p.shares {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// remove forgets a share; the caller stops its handle.
func (p *PublicShares) removeLocked(id string) *publicShare {
	for i, s := range p.shares {
		if s.ID == id {
			p.shares = append(p.shares[:i], p.shares[i+1:]...)
			return s
		}
	}
	return nil
}

// shareRequest is POST /fabo/share and /fabo/share/run.
type shareRequest struct {
	Target    string `json:"target"`
	Name      string `json:"name"`
	NoReserve bool   `json:"noReserve"`
}

// create resolves the target, reserves the name when asked, and starts a
// tunnel owned by owner ("app" or "cli").
func (p *PublicShares) create(ctx context.Context, in shareRequest, owner string, sub chan sharetunnel.Event) (*publicShare, error) {
	in.Target, in.Name = strings.TrimSpace(in.Target), strings.TrimSpace(in.Name)
	if in.Target == "" {
		return nil, fmt.Errorf("%w: a port, host:port or container is required", errInvalid)
	}
	target, ok := sharetunnel.ParseAddr(in.Target)
	var dial func(context.Context) (net.Conn, error)
	if ok {
		dial = sharetunnel.DialAddr(target.Addr)
	} else {
		if p.opts.ResolveContainer == nil || p.opts.DialContainer == nil {
			return nil, fmt.Errorf("%w: no port or container named %s", errMissing, in.Target)
		}
		name, err := p.opts.ResolveContainer(ctx, in.Target)
		if err != nil {
			return nil, err
		}
		target = sharetunnel.Target{Label: name, Display: name}
		dial = func(ctx context.Context) (net.Conn, error) { return p.opts.DialContainer(ctx, name) }
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("%w: the daemon is shutting down", errConflict)
	}
	srv, err := p.serverLocked()
	if err != nil {
		p.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", errInvalid, err)
	}
	token := p.cfg.EffectiveToken()
	if token == "" {
		p.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", errInvalid, sharetunnel.ErrSignedOut)
	}
	device := p.cfg.Device
	api := sharetunnel.NewAPI(srv, token, sharetunnel.UserAgent(p.opts.Version))
	p.mu.Unlock()

	s := &publicShare{
		ID: newShareID(), Target: target.Display, Name: in.Name, Owner: owner, State: sharetunnel.EventConnecting,
		Started: time.Now().UTC(), Recent: []sharetunnel.Request{}, subs: map[chan sharetunnel.Event]struct{}{},
		ended: make(chan struct{}),
	}
	// Reserving NAME first makes it the exact address; it never fails
	// the share.
	if in.Name != "" && !in.NoReserve {
		// Two relay calls; a slow cross-border link can take several
		// seconds per TLS handshake.
		rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		r := api.AutoReserve(rctx, in.Name)
		cancel()
		s.Reservation = &r
	}
	client := "Fabo " + p.opts.Version
	if owner == "cli" {
		client = "fabo " + p.opts.Version
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// The account may have changed while reserving.
	if p.closed || p.cfg.EffectiveToken() != token {
		return nil, fmt.Errorf("%w: the share account changed; try again", errConflict)
	}
	p.shares = append(p.shares, s)
	// A subscriber registered before Start sees every event from the first.
	if sub != nil {
		s.subs[sub] = struct{}{}
	}
	s.handle = sharetunnel.Start(sharetunnel.Options{
		Server: srv, Token: token, Device: device, Client: client, Target: target, Name: in.Name, Dial: dial,
		OnEvent: func(e sharetunnel.Event) { p.onEvent(s, e) },
	})
	return s, nil
}

// onEvent records a tunnel event and fans it out to subscribers.
func (p *PublicShares) onEvent(s *publicShare, e sharetunnel.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch e.Type {
	case sharetunnel.EventConnecting:
		if s.URL == "" {
			s.State = e.Type
		}
	case sharetunnel.EventLive:
		s.State, s.URL, s.Reason = e.Type, e.URL, ""
	case sharetunnel.EventReconnecting, sharetunnel.EventWaiting:
		s.State, s.Reason = e.Type, e.Reason
	case sharetunnel.EventEnded:
		s.State, s.Reason, s.Error = e.Type, e.Reason, e.Error
	case sharetunnel.EventRequest:
		r := *e.Request
		s.Requests++
		s.In += r.In
		s.Out += r.Out
		s.Recent = append(s.Recent, r)
		if len(s.Recent) > recentRequests {
			s.Recent = s.Recent[len(s.Recent)-recentRequests:]
		}
	}
	for ch := range s.subs {
		select {
		case ch <- e:
		default:
		}
	}
	if e.Type == sharetunnel.EventEnded {
		close(s.ended)
	}
}

// subscribe returns a channel of the share's events and its end signal.
func (p *PublicShares) subscribe(id string) (chan sharetunnel.Event, *publicShare, func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.find(id)
	if s == nil {
		return nil, nil, nil
	}
	ch := make(chan sharetunnel.Event, 256)
	s.subs[ch] = struct{}{}
	return ch, s, func() {
		p.mu.Lock()
		delete(s.subs, ch)
		p.mu.Unlock()
	}
}

// stop ends and forgets a share.
func (p *PublicShares) stop(id string) bool {
	p.mu.Lock()
	s := p.removeLocked(id)
	p.mu.Unlock()
	if s == nil {
		return false
	}
	s.handle.Stop()
	return true
}

func newShareID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// loginRequest is POST /fabo/share/login. Server switches the relay;
// Token, Dev (with Login) or GitHub picks how to sign in.
type loginRequest struct {
	Server *string `json:"server"`
	Token  string  `json:"token"`
	Dev    bool    `json:"dev"`
	Login  string  `json:"login"`
	GitHub bool    `json:"github"`
}

// switchServerLocked points the account at server. A token belongs to
// its relay, so switching clears it and stops the tunnels that used it.
func (p *PublicShares) switchServerLocked(server string) ([]*sharetunnel.Handle, error) {
	server = sharetunnel.NormalizeServer(server)
	if server != "" {
		if _, err := sharetunnel.ParseServer(server); err != nil {
			return nil, fmt.Errorf("%w: %v", errInvalid, err)
		}
	}
	before := sharetunnel.NormalizeServer(p.cfg.EffectiveServer())
	p.cfg.Server = server
	if sharetunnel.NormalizeServer(p.cfg.EffectiveServer()) == before {
		// Same relay in effect (or FABO_SHARE_SERVER pins it): keep the token.
		return nil, sharetunnel.SaveConfig(p.opts.ConfigPath, p.cfg)
	}
	p.cancelLoginLocked()
	p.cancelBillingLocked()
	handles := p.detachAllLocked()
	p.cfg.SetToken("", "")
	return handles, sharetunnel.SaveConfig(p.opts.ConfigPath, p.cfg)
}

// setLoginLocked saves new credentials. A different account (or any new
// token) replaces the old one, so the old tunnels stop first.
func (p *PublicShares) setLoginLocked(token, login string) ([]*sharetunnel.Handle, error) {
	p.cancelBillingLocked()
	handles := p.detachAllLocked()
	p.cfg.SetToken(token, login)
	return handles, sharetunnel.SaveConfig(p.opts.ConfigPath, p.cfg)
}

// signIn handles POST /fabo/share/login.
func (p *PublicShares) signIn(ctx context.Context, in loginRequest) (accountView, error) {
	var handles []*sharetunnel.Handle
	defer func() { stopAll(handles) }()
	p.mu.Lock()
	if in.Server != nil {
		stopped, err := p.switchServerLocked(*in.Server)
		handles = append(handles, stopped...)
		if err != nil {
			p.mu.Unlock()
			return accountView{}, err
		}
	}
	if !in.GitHub && !in.Dev && in.Token == "" {
		view := p.accountLocked()
		p.mu.Unlock()
		return view, nil
	}
	// Any new sign-in supersedes a device flow in progress.
	p.cancelLoginLocked()
	gen := p.loginGen
	srv, err := p.serverLocked()
	device := p.cfg.Device
	p.mu.Unlock()
	if err != nil {
		return accountView{}, fmt.Errorf("%w: %v", errInvalid, err)
	}
	api := sharetunnel.NewAPI(srv, "", sharetunnel.UserAgent(p.opts.Version))
	var token, login string
	switch {
	case in.Token != "":
		api.Token = strings.TrimSpace(in.Token)
		acct, err := api.Whoami(ctx)
		if err != nil {
			return accountView{}, fmt.Errorf("%w: %v", errInvalid, err)
		}
		token, login = api.Token, acct.Login
	case in.Dev:
		name := in.Login
		if name == "" {
			name = "dev"
		}
		l, err := api.LoginDev(ctx, name, device)
		if err != nil {
			return accountView{}, fmt.Errorf("%w: %v", errInvalid, err)
		}
		token, login = l.Token, l.Login
	default:
		code, err := api.LoginStart(ctx)
		if err != nil {
			return accountView{}, fmt.Errorf("%w: %v", errInvalid, err)
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.loginGen != gen {
			return p.accountLocked(), nil
		}
		pctx, cancel := context.WithTimeout(context.Background(), code.Lifetime())
		p.login = &pendingLogin{gen: gen, cancel: cancel, Code: code.UserCode, URI: code.VerificationURI}
		go p.pollLogin(pctx, cancel, api, code, device, gen)
		return p.accountLocked(), nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loginGen != gen {
		return p.accountLocked(), fmt.Errorf("%w: another sign-in replaced this one", errConflict)
	}
	stopped, err := p.setLoginLocked(token, login)
	handles = append(handles, stopped...)
	return p.accountLocked(), err
}

// pollLogin waits for the device code's approval; a result from a
// superseded sign-in (gen changed) is dropped.
func (p *PublicShares) pollLogin(ctx context.Context, cancel context.CancelFunc, api *sharetunnel.API, code sharetunnel.DeviceCode, device string, gen uint64) {
	defer cancel()
	wait := code.PollInterval()
	fail := func(msg string) {
		p.mu.Lock()
		if p.loginGen == gen && p.login != nil {
			p.login.Error = msg
		}
		p.mu.Unlock()
	}
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				fail("the sign-in code expired; sign in again")
			}
			return
		case <-time.After(wait):
		}
		login, pending, slowDown, err := api.LoginPoll(ctx, code.DeviceCode, device)
		if err != nil {
			if ctx.Err() == nil {
				fail(err.Error())
			}
			return
		}
		if pending {
			wait = code.PollInterval()
			if slowDown {
				wait += 5 * time.Second
			}
			continue
		}
		p.mu.Lock()
		if p.loginGen != gen {
			p.mu.Unlock()
			return
		}
		p.login = nil
		handles, err := p.setLoginLocked(login.Token, login.Login)
		p.mu.Unlock()
		stopAll(handles)
		if err != nil {
			fail(err.Error())
		}
		return
	}
}

// signOut handles POST /fabo/share/logout: the tunnels stop first.
func (p *PublicShares) signOut() (accountView, error) {
	p.mu.Lock()
	p.cancelLoginLocked()
	handles, err := p.setLoginLocked("", "")
	view := p.accountLocked()
	p.mu.Unlock()
	stopAll(handles)
	return view, err
}

// ServeHTTP serves /fabo/share and everything under it.
func (p *PublicShares) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(APIPath(r.URL.Path), "/")
	rest, _ := strings.CutPrefix(path, "/fabo/share")
	rest = strings.TrimPrefix(rest, "/")
	decode := func(v any) bool {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(v); err != nil {
			writeError(w, fmt.Errorf("%w: %v", errInvalid, err))
			return false
		}
		return true
	}
	switch {
	case rest == "" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		writeJSON(w, http.StatusOK, p.report())
	case rest == "" && r.Method == http.MethodPost:
		var in shareRequest
		if !decode(&in) {
			return
		}
		s, err := p.create(r.Context(), in, "app", nil)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": s.ID, "reservation": s.Reservation})
	case rest == "run" && r.Method == http.MethodPost:
		var in shareRequest
		if !decode(&in) {
			return
		}
		p.run(w, r, in)
	case rest == "login" && r.Method == http.MethodPost:
		var in loginRequest
		if !decode(&in) {
			return
		}
		view, err := p.signIn(r.Context(), in)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	case rest == "logout" && r.Method == http.MethodPost:
		view, err := p.signOut()
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	case rest == "account" && r.Method == http.MethodGet:
		p.relayCall(w, func(api *sharetunnel.API) (any, error) {
			acct, err := api.Whoami(r.Context())
			if err == nil {
				p.observeAccount(api, acct)
			}
			return acct, err
		})
	case rest == "billing" && r.Method == http.MethodPost:
		p.relayCall(w, func(api *sharetunnel.API) (any, error) {
			link, err := api.BillingURL(r.Context())
			if err == nil {
				p.startBillingPoll(api, 15*time.Second, 10*time.Minute)
			}
			return map[string]string{"url": link}, err
		})
	case rest == "names" && r.Method == http.MethodGet:
		p.relayCall(w, func(api *sharetunnel.API) (any, error) { return api.Names(r.Context()) })
	case rest == "names" && r.Method == http.MethodPost:
		var in struct {
			Name string `json:"name"`
		}
		if !decode(&in) {
			return
		}
		p.relayCall(w, func(api *sharetunnel.API) (any, error) { return api.Reserve(r.Context(), in.Name) })
	case strings.HasPrefix(rest, "names/") && r.Method == http.MethodDelete:
		name := strings.TrimPrefix(rest, "names/")
		p.relayCall(w, func(api *sharetunnel.API) (any, error) {
			return map[string]string{"released": name}, api.Release(r.Context(), name)
		})
	case strings.HasSuffix(rest, "/events") && r.Method == http.MethodGet:
		p.watch(w, r, strings.TrimSuffix(rest, "/events"))
	case rest != "" && !strings.Contains(rest, "/") && r.Method == http.MethodDelete:
		if !p.stop(rest) {
			writeError(w, fmt.Errorf("%w: no share %s", errMissing, rest))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "page not found"})
	}
}

// relayCall runs an account call against the relay, mapping its errors.
func (p *PublicShares) relayCall(w http.ResponseWriter, call func(*sharetunnel.API) (any, error)) {
	p.mu.Lock()
	api, err := p.apiLocked()
	p.mu.Unlock()
	if err != nil {
		writeError(w, fmt.Errorf("%w: %v", errInvalid, err))
		return
	}
	out, err := call(api)
	if err != nil {
		var se *sharetunnel.StatusError
		switch {
		case errors.As(err, &se):
			writeJSON(w, se.Status, map[string]string{"message": se.Message})
		case errors.Is(err, sharetunnel.ErrSessionExpired), errors.Is(err, sharetunnel.ErrSignedOut):
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": err.Error()})
		default:
			writeJSON(w, http.StatusBadGateway, map[string]string{"message": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// streamEvents writes events as NDJSON until the share ends or the
// request goes away; it reports whether the share ended.
func streamEvents(w http.ResponseWriter, r *http.Request, ch chan sharetunnel.Event, ended chan struct{}) bool {
	// The stream outlives the server's ReadTimeout; without clearing it the
	// connection's background read would time out and cancel the request,
	// which for /run would stop the tunnel.
	ctrl := http.NewResponseController(w)
	_ = ctrl.SetReadDeadline(time.Time{})
	_ = ctrl.SetWriteDeadline(time.Time{})
	enc := json.NewEncoder(w)
	flush := func() { _ = ctrl.Flush() }
	flush()
	for {
		select {
		case <-r.Context().Done():
			return false
		case e := <-ch:
			if enc.Encode(e) != nil {
				return false
			}
			flush()
			if e.Type == sharetunnel.EventEnded {
				return true
			}
		case <-ended:
			// Drain what is queued, ending with the ended event.
			for {
				select {
				case e := <-ch:
					_ = enc.Encode(e)
				default:
					flush()
					return true
				}
			}
		}
	}
}

// watch serves GET /fabo/share/{id}/events for observers; leaving does
// not affect the tunnel.
func (p *PublicShares) watch(w http.ResponseWriter, r *http.Request, id string) {
	ch, s, unsubscribe := p.subscribe(id)
	if ch == nil {
		writeError(w, fmt.Errorf("%w: no share %s", errMissing, id))
		return
	}
	defer unsubscribe()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	streamEvents(w, r, ch, s.ended)
}

// run serves POST /fabo/share/run for the CLI: it creates the share and
// streams its events, and the request itself is the share's lease — when
// it ends for any reason, the tunnel stops.
func (p *PublicShares) run(w http.ResponseWriter, r *http.Request, in shareRequest) {
	ch := make(chan sharetunnel.Event, 256)
	s, err := p.create(r.Context(), in, "cli", ch)
	if err != nil {
		writeError(w, err)
		return
	}
	defer p.stop(s.ID)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "created", "id": s.ID, "target": s.Target, "reservation": s.Reservation})
	streamEvents(w, r, ch, s.ended)
}
