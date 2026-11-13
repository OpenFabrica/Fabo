package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
)

type engineInfo struct {
	Name, ServerVersion                   string
	KiriVmAwake, KiriVMControls           *bool
	KiriStopped                           bool
	Containers, ContainersRunning, Images int
	// Reported only while a VM is awake.
	NCPU     int
	MemTotal int64
}

func (i engineInfo) status() string {
	switch {
	case i.KiriStopped:
		return "Stopped"
	case i.KiriVmAwake == nil:
		return "Connected"
	case *i.KiriVmAwake:
		return "Running"
	default:
		return "Asleep"
	}
}

type engineClient struct{ http *http.Client }

func newEngineClient(dial func(context.Context) (net.Conn, error)) engineClient {
	return engineClient{&http.Client{
		Transport: &http.Transport{
			DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (e engineClient) request(ctx context.Context, method, path string, input any, out any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, body)
	if err != nil {
		return err
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodGet {
		// Browsing must not extend the lifetime of an idle VM.
		req.Header.Set("X-Kiri-Background", "1")
	}
	res, err := e.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var problem struct{ Message string }
		_ = json.NewDecoder(io.LimitReader(res.Body, 65536)).Decode(&problem)
		if problem.Message != "" {
			return fmt.Errorf("%s", problem.Message)
		}
		return fmt.Errorf("Kiri returned HTTP %d", res.StatusCode)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, res.Body)
		return err
	}
	if raw, ok := out.(*[]byte); ok {
		*raw, err = io.ReadAll(io.LimitReader(res.Body, 256<<10))
		return err
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func (e engineClient) info(ctx context.Context) (engineInfo, error) {
	var info engineInfo
	if err := e.request(ctx, http.MethodGet, "/info", nil, &info); err != nil {
		return engineInfo{}, err
	}
	if info.Name != "kiri" && info.KiriVmAwake == nil {
		return engineInfo{}, fmt.Errorf("the endpoint did not identify itself as Kiri")
	}
	return info, nil
}

func logText(raw []byte, tty bool) string {
	if tty {
		return string(raw)
	}
	var text bytes.Buffer
	for len(raw) >= 8 {
		size := int(binary.BigEndian.Uint32(raw[4:8]))
		raw = raw[8:]
		if size > len(raw) {
			text.Write(raw)
			break
		}
		text.Write(raw[:size])
		raw = raw[size:]
	}
	return text.String()
}
