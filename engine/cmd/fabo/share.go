//go:build linux || darwin

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/sharetunnel"
)

const shareUsage = `usage: fabo share PORT|HOST:PORT|CONTAINER [NAME | --name NAME] [--no-reserve] [--json]
       fabo PORT [NAME]                  the same, for a port or host:port
       fabo share login [--server HOST] [--token TOKEN | --dev [LOGIN]]
       fabo share logout | status [--json] | names | reserve NAME | release NAME`

// shareCommand is `fabo share …` and the `fabo PORT` shorthand; the daemon
// hosts the tunnels and this command only drives them.
func shareCommand(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		return errors.New(shareUsage)
	}
	in, err := locate()
	if err != nil {
		return err
	}
	c, err := in.runningClient()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runShare(ctx, c, args, os.Stdout, os.Stderr)
}

// runShare dispatches a share subcommand against the daemon behind c.
func runShare(ctx context.Context, c *commandClient, args []string, out, errOut io.Writer) error {
	switch args[0] {
	case "login":
		return shareLogin(ctx, c, args[1:], out)
	case "logout":
		if len(args) != 1 {
			return errors.New(shareUsage)
		}
		if err := c.request("POST", "/fabo/share/logout", map[string]any{}, nil); err != nil {
			return err
		}
		fmt.Fprintln(out, "Signed out of Fabo Share; its links on this computer were closed.")
		return nil
	case "status":
		return shareStatus(c, args[1:], out)
	case "names":
		if len(args) != 1 {
			return errors.New(shareUsage)
		}
		return shareNames(c, out)
	case "reserve", "release":
		if len(args) != 2 || args[1] == "" {
			return errors.New(shareUsage)
		}
		return shareReserve(c, args[0], args[1], out)
	}
	return shareRun(ctx, c, args, out, errOut)
}

// shareOptions are the arguments of `fabo share TARGET …`.
type shareOptions struct {
	target, name    string
	noReserve, json bool
}

// parseShareArgs reads TARGET [NAME | --name NAME] [--no-reserve] [--json].
func parseShareArgs(args []string) (shareOptions, error) {
	var o shareOptions
	named := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--no-reserve":
			o.noReserve = true
		case a == "--json":
			o.json = true
		case a == "--name" || a == "-n":
			if named || i+1 >= len(args) {
				return o, fmt.Errorf("unexpected argument %q\n%s", a, shareUsage)
			}
			i++
			o.name, named = args[i], true
		case strings.HasPrefix(a, "--name="):
			if named {
				return o, fmt.Errorf("unexpected argument %q\n%s", a, shareUsage)
			}
			o.name, named = strings.TrimPrefix(a, "--name="), true
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown option %q\n%s", a, shareUsage)
		case o.target == "":
			o.target = a
		case !named:
			o.name, named = a, true
		default:
			return o, fmt.Errorf("unexpected argument %q\n%s", a, shareUsage)
		}
	}
	if o.target == "" {
		return o, errors.New(shareUsage)
	}
	return o, nil
}

// shareEvent is one NDJSON line from POST /fabo/share/run.
type shareEvent struct {
	sharetunnel.Event
	ID          string                   `json:"id"`
	Target      string                   `json:"target"`
	Reservation *sharetunnel.Reservation `json:"reservation"`
}

// errTunnelClosed reports a share that ended on its own, not by Ctrl+C.
var errTunnelClosed = errors.New("the tunnel closed")

// shareRun opens a tunnel and prints its events until Ctrl+C. The request
// is the tunnel's lease: when this process ends, the daemon closes it.
func shareRun(ctx context.Context, c *commandClient, args []string, out, errOut io.Writer) error {
	o, err := parseShareArgs(args)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(map[string]any{"target": o.target, "name": o.name, "noReserve": o.noReserve})
	req, err := http.NewRequestWithContext(ctx, "POST", "http://fabo/fabo/share/run", strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: c.http.Transport}).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return commandResponseError(resp)
	}
	p := &sharePrinter{out: out, errOut: errOut, json: o.json}
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(make([]byte, 64<<10), 1<<20)
	for lines.Scan() {
		var e shareEvent
		if err := json.Unmarshal(lines.Bytes(), &e); err != nil {
			continue
		}
		if o.json {
			if e.Type != "created" {
				fmt.Fprintln(out, lines.Text())
			}
		} else {
			p.print(e)
		}
		if e.Type == sharetunnel.EventEnded {
			if e.Error {
				return errTunnelClosed
			}
			return nil
		}
	}
	if ctx.Err() != nil {
		// Ctrl+C: closing the request closed the tunnel.
		return nil
	}
	return errors.New("lost the connection to the Fabo daemon; the tunnel closed")
}

// sharePrinter turns tunnel events into the terminal lines upstream prints.
type sharePrinter struct {
	out, errOut io.Writer
	json        bool
	target      string
	url         string
	connecting  bool
}

func (p *sharePrinter) print(e shareEvent) {
	switch e.Type {
	case "created":
		p.target = e.Target
		if r := e.Reservation; r != nil {
			if note := reservationNote(*r); note != "" {
				fmt.Fprintln(p.out, note)
			}
		}
	case sharetunnel.EventConnecting:
		if !p.connecting && p.url == "" {
			p.connecting = true
			fmt.Fprintf(p.out, "◆ Fabo Share connecting to %s…\n", e.Server)
		}
	case sharetunnel.EventLive:
		switch {
		case p.url == "":
			fmt.Fprintf(p.out, "● linked %s\n● live %s\n", p.target, e.URL)
			for _, d := range e.Domains {
				fmt.Fprintf(p.out, "● domain %s\n", d)
			}
			if e.Rate > 0 {
				fmt.Fprintf(p.out, "  %s/s per direction\n", humanBytes(uint64(e.Rate)))
			}
			fmt.Fprintln(p.out, "Requests show up below. Press Ctrl+C to stop sharing.")
		case p.url == e.URL:
			fmt.Fprintln(p.out, "● back online")
		default:
			fmt.Fprintf(p.out, "● new address: %s\n", e.URL)
		}
		p.url = e.URL
	case sharetunnel.EventRequest:
		if r := e.Request; r != nil {
			fmt.Fprintln(p.out, requestLine(*r))
		}
	case sharetunnel.EventNotice:
		fmt.Fprintf(p.out, "! %s\n", e.Text)
	case sharetunnel.EventReconnecting:
		fmt.Fprintf(p.out, "↻ %s — reconnecting in %.1fs\n", e.Reason, float64(e.DelayMS)/1000)
	case sharetunnel.EventEnded:
		if e.Error {
			fmt.Fprintf(p.errOut, "✕ %s\n", e.Reason)
		}
	}
}

// reservationNote says what happened to the requested name.
func reservationNote(r sharetunnel.Reservation) string {
	switch r.Kind {
	case "reserved":
		return fmt.Sprintf("Reserved %s for you; it's yours until you release it.", r.URL)
	case "requested":
		return fmt.Sprintf("Asked for %s; until it's approved this link gets a random address.", r.URL)
	case "waiting":
		return "That name is waiting for review; this link gets a random address for now."
	case "full":
		return fmt.Sprintf("Your plan's %d reserved name(s) are in use; this link gets a random address. See `fabo share names`.", r.Limit)
	case "unavailable":
		return "That name isn't available; the relay picks the address."
	}
	return ""
}

// requestLine is `HH:MM:SS METHOD STATUS PATH  N ms  SIZE`.
func requestLine(r sharetunnel.Request) string {
	status := "---"
	if r.Status != 0 {
		status = fmt.Sprint(r.Status)
	}
	path := r.Path
	if len(path) > 40 {
		path = path[:39] + "…"
	}
	return fmt.Sprintf("%s %-6s %s %-40s %5d ms %8s", r.At.Local().Format("15:04:05"), r.Method, status, path, r.MS, humanBytes(r.Out))
}

// humanBytes formats n as B, KB, MB or GB.
func humanBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// shareAccount is the account part of GET /fabo/share.
type shareAccount struct {
	SignedIn bool   `json:"signedIn"`
	Login    string `json:"login"`
	Server   string `json:"server"`
	Pending  *struct {
		Code  string `json:"code"`
		URI   string `json:"uri"`
		Error string `json:"error"`
	} `json:"pending"`
}

// openURL opens a sign-in page in the browser; tests replace it.
var openURL = func(u string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "/usr/bin/open"
	}
	return exec.Command(name, u).Start()
}

// shareLogin signs in: with a token, as a development login, or (by
// default) with the GitHub device flow, which the daemon polls.
func shareLogin(ctx context.Context, c *commandClient, args []string, out io.Writer) error {
	body := map[string]any{}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--server" && i+1 < len(args):
			i++
			body["server"] = args[i]
		case a == "--token" && i+1 < len(args):
			i++
			body["token"] = args[i]
		case a == "--dev":
			body["dev"] = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				body["login"] = args[i]
			}
		default:
			return errors.New(shareUsage)
		}
	}
	_, token := body["token"]
	_, dev := body["dev"]
	if !token && !dev {
		body["github"] = true
	}
	var account shareAccount
	if err := c.request("POST", "/fabo/share/login", body, &account); err != nil {
		return err
	}
	if account.Pending == nil {
		fmt.Fprintf(out, "● Signed in to Fabo Share (%s) as %s.\n", account.Server, account.Login)
		return nil
	}
	uri := account.Pending.URI
	fmt.Fprintf(out, "◆ Sign in to Fabo Share with GitHub\n  Open %s and enter %s\n", uri, account.Pending.Code)
	if u, err := url.Parse(uri); err == nil && u.Scheme == "https" {
		_ = openURL(uri)
	}
	fmt.Fprintln(out, "Waiting for you to approve…")
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
		var report struct {
			Account shareAccount `json:"account"`
		}
		if err := c.request("GET", "/fabo/share", nil, &report); err != nil {
			return err
		}
		a := report.Account
		switch {
		case a.Pending != nil && a.Pending.Error != "":
			return errors.New(a.Pending.Error)
		case a.Pending == nil && a.SignedIn:
			fmt.Fprintf(out, "● Signed in as %s.\n", a.Login)
			return nil
		case a.Pending == nil:
			return errors.New("the sign-in was cancelled")
		}
	}
}

// shareStatus prints the account, its limits and this engine's links.
func shareStatus(c *commandClient, args []string, out io.Writer) error {
	asJSON := len(args) == 1 && args[0] == "--json"
	if len(args) > 1 || len(args) == 1 && !asJSON {
		return errors.New(shareUsage)
	}
	var report struct {
		Account shareAccount `json:"account"`
		Shares  []struct {
			ID, Target, URL, State, Owner, Reason string
			Requests                              uint64
		} `json:"shares"`
	}
	if err := c.request("GET", "/fabo/share", nil, &report); err != nil {
		return err
	}
	a := report.Account
	if asJSON {
		return json.NewEncoder(out).Encode(map[string]any{"signed_in": a.SignedIn, "server": a.Server, "login": a.Login, "shares": report.Shares})
	}
	if !a.SignedIn {
		where := a.Server
		if where == "" {
			where = "no relay set"
		}
		fmt.Fprintf(out, "Not signed in to Fabo Share (%s). Run `fabo share login`.\n", where)
		return nil
	}
	fmt.Fprintf(out, "◆ Fabo Share (%s)\n  account   %s\n", a.Server, a.Login)
	var acct sharetunnel.Account
	if err := c.request("GET", "/fabo/share/account", nil, &acct); err == nil {
		if acct.Limits.Tunnels != nil {
			fmt.Fprintf(out, "  tunnels   up to %d at once\n", *acct.Limits.Tunnels)
		} else {
			fmt.Fprintln(out, "  tunnels   no limit")
		}
		if acct.Limits.Rate > 0 {
			fmt.Fprintf(out, "  speed     %s/s\n", humanBytes(acct.Limits.Rate))
		}
	} else {
		fmt.Fprintf(out, "  (%v)\n", err)
	}
	for _, s := range report.Shares {
		mark := "○"
		if s.State == sharetunnel.EventLive {
			mark = "●"
		}
		where := s.URL
		if where == "" {
			where = s.State
		}
		fmt.Fprintf(out, "%s %s ← %s  (%d requests)\n", mark, where, s.Target, s.Requests)
	}
	return nil
}

// shareNames lists the account's reserved names.
func shareNames(c *commandClient, out io.Writer) error {
	var names sharetunnel.Names
	if err := c.request("GET", "/fabo/share/names", nil, &names); err != nil {
		return err
	}
	used := fmt.Sprintf("%d used, no limit", len(names.Items))
	if names.Limit != nil {
		used = fmt.Sprintf("%d of %d used", len(names.Items), *names.Limit)
	}
	fmt.Fprintf(out, "◆ Reserved names  %s\n", used)
	for _, n := range names.Items {
		mark := "○ idle"
		switch {
		case n.Pending():
			mark = "◌ waiting"
		case n.InUse:
			mark = "● live"
		}
		fmt.Fprintf(out, "  %-10s %-24s %s\n", mark, n.Name, n.URL)
	}
	if len(names.Refused) > 0 {
		fmt.Fprintln(out, "Not approved:")
		for _, r := range names.Refused {
			fmt.Fprintf(out, "  %s: %s\n", r.Name, r.Reason)
		}
	}
	return nil
}

// shareReserve reserves or releases one name.
func shareReserve(c *commandClient, action, name string, out io.Writer) error {
	if action == "release" {
		if err := c.request("DELETE", "/fabo/share/names/"+url.PathEscape(strings.ToLower(name)), nil, nil); err != nil {
			return err
		}
		fmt.Fprintf(out, "Released `%s`. Anyone can ask for it now.\n", name)
		return nil
	}
	var r sharetunnel.ReservedName
	if err := c.request("POST", "/fabo/share/names", map[string]string{"name": name}, &r); err != nil {
		return err
	}
	if r.Pending() {
		fmt.Fprintf(out, "◌ Asked for %s; it's yours once approved.\n", r.URL)
		return nil
	}
	fmt.Fprintf(out, "● %s is yours: %s\n", r.Name, r.URL)
	return nil
}
