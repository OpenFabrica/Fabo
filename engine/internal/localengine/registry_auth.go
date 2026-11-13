//go:build linux || darwin

package localengine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

func registryAuth(r *http.Request) (authn.AuthConfig, error) {
	var auth authn.AuthConfig
	if encoded := r.Header.Get("X-Registry-Auth"); encoded != "" {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(encoded, "="))
		if err != nil || json.Unmarshal(raw, &auth) != nil {
			return authn.AuthConfig{}, fmt.Errorf("%w: invalid registry authentication header", errInvalid)
		}
	}
	return auth, nil
}

func (e *Engine) registryLoginHTTP(w http.ResponseWriter, r *http.Request) {
	var auth authn.AuthConfig
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	// AuthConfig has a custom decoder, so decode the address separately.
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil ||
		json.Unmarshal(raw, &auth) != nil {
		writeError(w, fmt.Errorf("%w: invalid login request", errInvalid))
		return
	}
	var server struct {
		Address string `json:"serveraddress"`
	}
	if err := json.Unmarshal(raw, &server); err != nil {
		writeError(w, fmt.Errorf("%w: invalid registry address", errInvalid))
		return
	}
	reg, err := loginRegistry(server.Address)
	if err != nil {
		writeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := e.checkRegistryLogin(ctx, reg, auth); err != nil {
		if registryDenied(err) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "unauthorized: incorrect username or password"})
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "login failed for " + reg.RegistryStr()})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"Status": "Login Succeeded", "IdentityToken": ""})
}

func loginRegistry(address string) (name.Registry, error) {
	host := name.DefaultRegistry
	if address != "" {
		if !strings.Contains(address, "://") {
			address = "https://" + address
		}
		u, err := url.Parse(address)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" ||
			u.Fragment != "" || u.Scheme != "https" && u.Scheme != "http" {
			return name.Registry{}, fmt.Errorf("%w: invalid registry address", errInvalid)
		}
		host = u.Host
	}
	if host == name.DefaultRegistry || host == "docker.io" {
		host = "registry-1.docker.io"
	}
	registry, err := name.NewRegistry(host, name.StrictValidation)
	if err != nil {
		return name.Registry{}, fmt.Errorf("%w: invalid registry address", errInvalid)
	}
	return registry, nil
}

func (e *Engine) checkRegistryLogin(ctx context.Context, registry name.Registry, auth authn.AuthConfig) error {
	tr, err := transport.NewWithContext(ctx, registry, authn.FromConfig(auth), e.registryTransport(), nil)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, registry.Scheme()+"://"+registry.RegistryStr()+"/v2/", nil)
	if err != nil {
		return err
	}
	client := http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &transport.Error{StatusCode: response.StatusCode}
	}
	return nil
}

func registryDenied(err error) bool {
	var failure *transport.Error
	return errors.As(err, &failure) && (failure.StatusCode == http.StatusUnauthorized || failure.StatusCode == http.StatusForbidden)
}

func writeRegistryStreamError(out *json.Encoder, err error, denied string) {
	message := err.Error()
	if registryDenied(err) {
		message = denied + ": authentication required or access denied"
	}
	_ = out.Encode(map[string]any{"error": message, "errorDetail": map[string]string{"message": message}})
}
