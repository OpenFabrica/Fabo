package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

var errRegistryNotFound = errors.New("registry credentials not found")

const dockerHub = "https://index.docker.io/v1/"
const tokenUsername = "<token>"

type registryLogin struct{ Username, Secret string }
type registryAccount struct{ Server, Username, Store string }

// Native storage uses Docker's credential records. Helpers take precedence,
// followed by native storage and existing inline Docker credentials.
type nativeRegistryCredentials interface {
	get(string) (registryLogin, error)
	set(string, registryLogin) error
	remove(string) error
	list() (map[string]string, error)
}
type registryStore struct {
	path   string
	native nativeRegistryCredentials
	helper func(context.Context, string, string, []byte) ([]byte, error)
}

func newRegistryStore() *registryStore {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".docker")
		}
	}
	path := ""
	if dir != "" {
		path = filepath.Join(dir, "config.json")
	}
	return &registryStore{path: path, native: newNativeRegistryCredentials(), helper: runCredentialHelper}
}

func registryKey(server string) (string, error) {
	server = strings.ToLower(strings.TrimSpace(server))
	if server == "" {
		return dockerHub, nil
	}
	if !strings.Contains(server, "://") {
		server = "https://" + server
	}
	u, err := url.Parse(server)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") || strings.ContainsAny(u.Host, " \t\r\n\\") || strings.HasPrefix(u.Host, "-") {
		return "", errors.New("invalid registry address")
	}
	// Only Docker Hub's historical /v1/ path is meaningful in a registry key.
	if u.Path != "" && u.Path != "/" && !(u.Hostname() == "index.docker.io" && u.Path == "/v1/") {
		return "", errors.New("a registry address must be a host, optionally with a port")
	}
	switch u.Host {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		return dockerHub, nil
	}
	if p := u.Port(); p != "" {
		if _, err := net.LookupPort("tcp", p); err != nil {
			return "", errors.New("invalid registry port")
		}
	}
	return u.Host, nil
}
func registryForImage(ref string) (string, error) {
	first, _, hasSlash := strings.Cut(strings.TrimSpace(ref), "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return registryKey(first)
	}
	return dockerHub, nil
}
func registryDisplay(key string) string {
	if key == dockerHub {
		return "Docker Hub"
	}
	return key
}

func (s *registryStore) config() (map[string]json.RawMessage, error) {
	if s.path == "" {
		return nil, errors.New("Docker configuration directory is unavailable")
	}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg map[string]json.RawMessage
	if err = json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &cfg); err != nil || cfg == nil {
		return nil, errors.New("Docker config.json is not a JSON object")
	}
	return cfg, nil
}
func authEntries(cfg map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	auths := map[string]json.RawMessage{}
	if raw := cfg["auths"]; raw != nil {
		if err := json.Unmarshal(raw, &auths); err != nil || auths == nil {
			return nil, errors.New("Docker config.json auths must be an object")
		}
	}
	return auths, nil
}
func helperName(cfg map[string]json.RawMessage, key string) (string, error) {
	var own map[string]string
	if raw := cfg["credHelpers"]; raw != nil {
		if err := json.Unmarshal(raw, &own); err != nil {
			return "", errors.New("invalid Docker credHelpers")
		}
	}
	name := ""
	for server, v := range own {
		k, err := registryKey(server)
		if err == nil && k == key {
			name = v
			break
		}
	}
	if name == "" && cfg["credsStore"] != nil {
		if err := json.Unmarshal(cfg["credsStore"], &name); err != nil {
			return "", errors.New("invalid Docker credsStore")
		}
	}
	if name == "" {
		return "", nil
	}
	if strings.HasPrefix(name, ".") {
		return "", errors.New("invalid Docker credential helper name")
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.", c)) {
			return "", errors.New("invalid Docker credential helper name")
		}
	}
	return name, nil
}
func runCredentialHelper(ctx context.Context, name, action string, input []byte) ([]byte, error) {
	filename := "docker-credential-" + name
	path, err := exec.LookPath(filename)
	if err != nil {
		exe, _ := os.Executable()
		home, _ := os.UserHomeDir()
		dirs := []string{"/usr/local/bin", "/opt/homebrew/bin", "/Applications/Docker.app/Contents/Resources/bin", filepath.Join(home, ".docker/bin"), filepath.Join(home, ".kiri/bin"), filepath.Join(home, "Applications/Docker.app/Contents/Resources/bin"), filepath.Join(filepath.Dir(exe), "../Resources/bin")}
		for _, dir := range dirs {
			if found, e := exec.LookPath(filepath.Join(dir, filename)); e == nil {
				path, err = found, nil
				break
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("docker-credential-%s was not found", name)
	}
	cmd := exec.CommandContext(ctx, path, action)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	// Helpers receive secrets. Their raw output/stderr must not become an error toast.
	if err != nil {
		if action == "get" && strings.Contains(strings.ToLower(string(out)), "credentials not found") {
			return nil, errRegistryNotFound
		}
		return nil, fmt.Errorf("docker-credential-%s could not %s credentials", name, action)
	}
	return out, nil
}

func decodeInline(raw json.RawMessage) (registryLogin, bool) {
	var entry struct{ Auth, IdentityToken, Username, Password string }
	if json.Unmarshal(raw, &entry) != nil {
		return registryLogin{}, false
	}
	if entry.IdentityToken != "" {
		return registryLogin{tokenUsername, entry.IdentityToken}, true
	}
	if entry.Auth != "" {
		b, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err == nil {
			u, p, ok := strings.Cut(string(b), ":")
			if ok {
				return registryLogin{u, p}, true
			}
		}
	}
	return registryLogin{entry.Username, entry.Password}, entry.Password != ""
}

func (s *registryStore) find(ctx context.Context, key string) (registryLogin, bool, error) {
	cfg, err := s.config()
	if err != nil {
		return registryLogin{}, false, err
	}
	helper, err := helperName(cfg, key)
	if err != nil {
		return registryLogin{}, false, err
	}
	if helper != "" {
		out, e := s.helper(ctx, helper, "get", []byte(key))
		if e != nil && !errors.Is(e, errRegistryNotFound) {
			return registryLogin{}, false, e
		}
		if e == nil {
			var record struct{ Username, Secret string }
			if json.Unmarshal(out, &record) != nil {
				return registryLogin{}, false, fmt.Errorf("docker-credential-%s returned invalid credentials", helper)
			}
			if record.Secret != "" {
				return registryLogin{record.Username, record.Secret}, true, nil
			}
		}
	} else if s.native != nil {
		login, e := s.native.get(key)
		if e != nil && !errors.Is(e, errRegistryNotFound) {
			return registryLogin{}, false, e
		}
		if e == nil && login.Secret != "" {
			return login, true, nil
		}
	}
	auths, err := authEntries(cfg)
	if err != nil {
		return registryLogin{}, false, err
	}
	for server, raw := range auths {
		if k, e := registryKey(server); e == nil && k == key {
			login, ok := decodeInline(raw)
			return login, ok, nil
		}
	}
	return registryLogin{}, false, nil
}

// Change only auths and atomically replace a valid config. Preserve unknown
// keys, exact JSON numbers and a user's symlinked Docker configuration.
func (s *registryStore) editAuths(cfg map[string]json.RawMessage, key string, remove bool) error {
	auths, err := authEntries(cfg)
	if err != nil {
		return err
	}
	for server := range auths {
		if k, e := registryKey(server); e == nil && k == key {
			delete(auths, server)
		}
	}
	if !remove {
		auths[key] = json.RawMessage(`{}`)
	}
	cfg["auths"], err = json.Marshal(auths)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "\t")
	if err != nil {
		return err
	}
	path := s.path
	if target, e := filepath.EvalSymlinks(path); e == nil {
		path = target
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fabo-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *registryStore) save(ctx context.Context, key string, login registryLogin) error {
	cfg, err := s.config()
	if err != nil {
		return err
	}
	if _, err = authEntries(cfg); err != nil {
		return err
	}
	helper, err := helperName(cfg, key)
	if err != nil {
		return err
	}
	if helper != "" {
		data, _ := json.Marshal(map[string]string{"ServerURL": key, "Username": login.Username, "Secret": login.Secret})
		if _, err = s.helper(ctx, helper, "store", data); err != nil {
			return err
		}
	} else if s.native != nil {
		if err = s.native.set(key, login); err != nil {
			return err
		}
	} else {
		return errors.New("configure a Docker credential helper before signing in")
	}
	if err = s.editAuths(cfg, key, false); err != nil {
		return fmt.Errorf("credentials saved, but Docker config could not be updated: %w", err)
	}
	return nil
}
func (s *registryStore) logout(ctx context.Context, key string) error {
	cfg, err := s.config()
	if err != nil {
		return err
	}
	if _, err = authEntries(cfg); err != nil {
		return err
	}
	helper, err := helperName(cfg, key)
	if err != nil {
		return err
	}
	if helper != "" {
		if _, err = s.helper(ctx, helper, "erase", []byte(key)); err != nil {
			return err
		}
	} else if s.native != nil {
		if err = s.native.remove(key); err != nil {
			return err
		}
	}
	return s.editAuths(cfg, key, true)
}
func (s *registryStore) accounts(ctx context.Context) ([]registryAccount, error) {
	cfg, err := s.config()
	if err != nil {
		return nil, err
	}
	auths, err := authEntries(cfg)
	if err != nil {
		return nil, err
	}
	var global string
	if cfg["credsStore"] != nil && json.Unmarshal(cfg["credsStore"], &global) != nil {
		return nil, errors.New("invalid Docker credsStore")
	}
	items := map[string]registryAccount{}
	// Native list reads attributes, never passwords or an unlock prompt.
	if s.native != nil && global == "" {
		native, e := s.native.list()
		if e != nil {
			return nil, e
		}
		for key, user := range native {
			selected, e := helperName(cfg, key)
			if e != nil {
				return nil, e
			}
			if selected == "" {
				items[key] = registryAccount{key, user, tr("系统钥匙串", "System credential store")}
			}
		}
	}
	for key, raw := range auths {
		if login, ok := decodeInline(raw); ok {
			if k, e := registryKey(key); e == nil {
				items[k] = registryAccount{k, login.Username, "config.json"}
			}
		}
	}
	helpers := map[string]bool{}
	if global != "" {
		helpers[global] = true
	}
	var own map[string]string
	if cfg["credHelpers"] != nil && json.Unmarshal(cfg["credHelpers"], &own) != nil {
		return nil, errors.New("invalid Docker credHelpers")
	}
	for key := range own {
		if k, e := registryKey(key); e == nil {
			h, e := helperName(cfg, k)
			if e != nil {
				return nil, e
			}
			if h != "" {
				helpers[h] = true
			}
		}
	}
	for h := range helpers {
		// Validate names even when they come from the global store.
		encoded, _ := json.Marshal(h)
		check := map[string]json.RawMessage{"credsStore": encoded}
		if _, e := helperName(check, dockerHub); e != nil {
			return nil, e
		}
		out, e := s.helper(ctx, h, "list", nil)
		if e != nil {
			return nil, e
		}
		var accounts map[string]string
		if json.Unmarshal(out, &accounts) != nil {
			return nil, fmt.Errorf("docker-credential-%s returned an invalid account list", h)
		}
		for server, user := range accounts {
			key, e := registryKey(server)
			if e != nil {
				continue
			}
			selected, e := helperName(cfg, key)
			if e != nil {
				return nil, e
			}
			if selected == h {
				items[key] = registryAccount{key, user, "docker-credential-" + h}
			}
		}
	}
	result := make([]registryAccount, 0, len(items))
	for _, item := range items {
		if item.Username == tokenUsername {
			item.Username = ""
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Server == dockerHub {
			return true
		}
		if result[j].Server == dockerHub {
			return false
		}
		return result[i].Server < result[j].Server
	})
	return result, nil
}

func registryHeader(key string, login registryLogin) string {
	auth := map[string]string{"serveraddress": key}
	if login.Username == tokenUsername {
		auth["identitytoken"] = login.Secret
	} else {
		auth["username"] = login.Username
		auth["password"] = login.Secret
	}
	raw, _ := json.Marshal(auth)
	return base64.URLEncoding.EncodeToString(raw)
}

func (e engineClient) registryLogin(ctx context.Context, store *registryStore, server, user, secret string) error {
	key, err := registryKey(server)
	if err != nil {
		return err
	}
	user = strings.TrimSpace(user)
	if user == "" || secret == "" || strings.ContainsAny(user, "\x00\r\n") || strings.ContainsRune(secret, 0) {
		return errors.New("enter a username and a token or password")
	}
	// Validate the registry before writing any credential or configuration.
	var response struct{ IdentityToken string }
	if err = e.request(ctx, http.MethodPost, "/auth", map[string]string{"serveraddress": key, "username": user, "password": secret}, &response); err != nil {
		return err
	}
	login := registryLogin{user, secret}
	if response.IdentityToken != "" {
		login = registryLogin{tokenUsername, response.IdentityToken}
	}
	return store.save(ctx, key, login)
}

type registryUI struct {
	loaded, loading                   bool
	accounts                          []registryAccount
	problem, server, username, secret string
}

func (s *desktop) loadRegistries() {
	if s.registries.loading {
		return
	}
	s.registries.loading = true
	s.run(30*time.Second, func(ctx context.Context) func() {
		accounts, err := s.credentials.accounts(ctx)
		return func() {
			s.registries.loading = false
			s.registries.loaded = true
			s.registries.problem = ""
			if err != nil {
				s.registries.problem = err.Error()
			} else {
				s.registries.accounts = accounts
			}
		}
	})
}
func (s *desktop) registryPane(c *ui.Context, p palette) {
	if !s.registries.loaded && !s.registries.loading {
		s.loadRegistries()
	}
	busy := s.working["registry"] || s.registries.loading
	heading(c, p, tr("镜像仓库", "Registries"))
	remark(c, p, tr("与 Docker 共用登录信息。凭据保存在配置的 credential helper 或系统钥匙串中。", "Share sign-ins with Docker. Credentials are kept in the configured credential helper or system credential store."))
	for _, account := range s.registries.accounts {
		pref(c.Key(account.Server), p, registryDisplay(account.Server), account.Username+" · "+account.Store, func() {
			if button(c, p, tr("退出登录…", "Sign out…"), "", plainButton, busy) {
				s.pending = &confirmation{tr("退出仓库登录？", "Sign out of registry?"), registryDisplay(account.Server), tr("退出登录", "Sign out"), func() { s.registryLogout(account.Server) }}
			}
		})
	}
	if s.registries.problem != "" {
		remark(c, p, s.registries.problem)
	}
	if button(c.Key("refresh-registries"), p, tr("刷新登录信息", "Refresh sign-ins"), "", plainButton, busy) {
		s.loadRegistries()
	}
	input(c, p, "registry-server", tr("仓库地址", "Registry address"), "docker.io", &s.registries.server, false).Disabled(busy)
	input(c, p, "registry-user", tr("用户名", "Username"), "", &s.registries.username, false).Disabled(busy)
	input(c, p, "registry-secret", tr("密码或令牌", "Password or token"), "", &s.registries.secret, false).Password().Disabled(busy)
	if button(c.Key("registry-login"), p, tr("登录", "Sign in"), "", primaryButton, busy || !s.connected) {
		s.registrySignIn()
	}
}
func (s *desktop) registrySignIn() {
	if s.working["registry"] {
		return
	}
	server, user, secret := s.registries.server, s.registries.username, s.registries.secret
	s.registries.secret = ""
	s.registries.problem = ""
	s.working["registry"] = true
	s.run(45*time.Second, func(ctx context.Context) func() {
		err := s.engine.registryLogin(ctx, s.credentials, server, user, secret)
		return func() {
			delete(s.working, "registry")
			if err != nil {
				s.registries.problem = err.Error()
			} else {
				s.registries.username = ""
				s.loadRegistries()
				s.notices = append(s.notices, tr("已登录镜像仓库", "Signed in to registry"))
			}
		}
	})
}
func (s *desktop) registryLogout(server string) {
	s.working["registry"] = true
	s.run(30*time.Second, func(ctx context.Context) func() {
		err := s.credentials.logout(ctx, server)
		return func() {
			delete(s.working, "registry")
			if err != nil {
				s.registries.problem = err.Error()
			} else {
				s.loadRegistries()
			}
		}
	})
}
