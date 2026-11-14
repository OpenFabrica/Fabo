//go:build linux || darwin

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// commandClient serves Fabo's own commands, which can take longer than the
// launcher's health checks. Every request targets this installation's socket.
type commandClient struct {
	socket string
	http   *http.Client
}

func newCommandClient(socket string) *commandClient {
	client := socketClient(socket)
	client.Timeout = 120 * time.Second
	return &commandClient{socket: socket, http: client}
}

type commandError struct {
	status  int
	message string
}

func (e *commandError) Error() string { return e.message }

func commandResponseError(resp *http.Response) error {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: %w", resp.Status, err)
	}
	var body struct{ Message string }
	_ = json.Unmarshal(raw, &body)
	message := body.Message
	if message == "" {
		message = strings.TrimSpace(string(raw))
	}
	if message == "" {
		message = resp.Status
	}
	return &commandError{status: resp.StatusCode, message: message}
}

// request checks the HTTP status before decoding JSON (or returning raw bytes).
func (c *commandClient) request(method, path string, body, out any) error {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequest(method, "http://fabo"+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return commandResponseError(resp)
	}
	if raw, ok := out.(*[]byte); ok {
		*raw, err = io.ReadAll(resp.Body)
		return err
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

// stream sends a request whose body or answer may be gigabytes, so it has no
// overall deadline; the caller closes the returned body. size is the body's
// length, or -1 when unknown.
func (c *commandClient) stream(method, path, contentType string, body io.Reader, size int64) (io.ReadCloser, error) {
	req, err := http.NewRequest(method, "http://fabo"+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := (&http.Client{Transport: c.http.Transport}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, commandResponseError(resp)
	}
	return resp.Body, nil
}

// exec is for bounded setup commands, not an interactive shell. TTY output
// is unframed; Docker handles the user's interactive terminal separately.
func (c *commandClient) exec(container string, command []string) (int, string, error) {
	var created struct{ ID string }
	body := map[string]any{"Cmd": command, "AttachStdout": true, "AttachStderr": true, "Tty": true}
	if err := c.request("POST", "/containers/"+url.PathEscape(container)+"/exec", body, &created); err != nil {
		return -1, "", err
	}
	if created.ID == "" {
		return -1, "", fmt.Errorf("engine returned an empty exec ID")
	}
	conn, err := net.DialTimeout("unix", c.socket, c.http.Timeout)
	if err != nil {
		return -1, "", err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(c.http.Timeout)); err != nil {
		return -1, "", err
	}
	path := "/exec/" + url.PathEscape(created.ID)
	req, err := http.NewRequest("POST", "http://fabo"+path+"/start", strings.NewReader(`{"Detach":false,"Tty":true}`))
	if err != nil {
		return -1, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if err := req.Write(conn); err != nil {
		return -1, "", err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return -1, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		return -1, "", commandResponseError(resp)
	}
	var source io.Reader = resp.Body
	if resp.StatusCode == http.StatusSwitchingProtocols {
		source = reader
	}
	raw, err := io.ReadAll(source)
	if err != nil {
		return -1, "", err
	}
	var inspected struct {
		ExitCode *int
		Running  bool
	}
	if err := c.request("GET", path+"/json", nil, &inspected); err != nil {
		return -1, "", err
	}
	if inspected.Running || inspected.ExitCode == nil {
		return -1, "", fmt.Errorf("exec ended without an exit status")
	}
	return *inspected.ExitCode, strings.ReplaceAll(string(raw), "\r\n", "\n"), nil
}
