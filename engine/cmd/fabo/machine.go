//go:build linux || darwin

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

const machineLabel = "dev.fabo.machine"
const machineUsage = `usage: fabo machine create NAME [--distro ubuntu|debian|alpine|fedora|arch|rocky|opensuse | --image IMAGE] [--systemd]
       fabo machine list | shell NAME [-- CMD...] | start NAME | stop NAME | restart NAME | delete NAME`

var machineDistros = map[string]string{
	"ubuntu": "ubuntu:24.04", "debian": "debian:trixie", "alpine": "alpine:3.22",
	"fedora": "fedora:42", "arch": "archlinux:latest", "rocky": "rockylinux:9",
	"opensuse": "opensuse/tumbleweed:latest",
}

type machineOptions struct {
	verb, name, image string
	systemd           bool
	command           []string
}

// Validate before contacting the engine, especially before pulling or deleting.
func parseMachine(args []string) (machineOptions, error) {
	op := machineOptions{image: "ubuntu:24.04"}
	bad := func() (machineOptions, error) { return op, errors.New(machineUsage) }
	if len(args) == 0 {
		return bad()
	}
	op.verb = args[0]
	switch op.verb {
	case "ls":
		op.verb = "list"
	case "rm":
		op.verb = "delete"
	}
	if op.verb == "list" {
		if len(args) != 1 {
			return bad()
		}
		return op, nil
	}
	switch op.verb {
	case "create", "shell", "start", "stop", "restart", "delete":
	default:
		return bad()
	}
	if len(args) < 2 || args[1] == "" {
		return op, errors.New("name a machine")
	}
	op.name = args[1]
	if !validMachineName(op.name) {
		return op, errors.New("a machine name is letters, digits and dashes, starting with a letter or digit")
	}
	rest := args[2:]
	if op.verb == "shell" && len(rest) > 0 && rest[0] == "--" {
		op.command = rest[1:]
		return op, nil
	}
	selected := false
	for len(rest) > 0 {
		if op.verb != "create" {
			return bad()
		}
		switch rest[0] {
		case "--systemd":
			op.systemd = true
			rest = rest[1:]
		case "--distro", "--image":
			if selected || len(rest) < 2 || rest[1] == "" || strings.HasPrefix(rest[1], "-") {
				return bad()
			}
			selected = true
			op.image = rest[1]
			// Upstream accepts custom images through either option.
			if image, ok := machineDistros[op.image]; ok {
				op.image = image
			}
			rest = rest[2:]
		default:
			return bad()
		}
	}
	return op, nil
}

func validMachineName(name string) bool {
	if name == "" || name[0] == '-' {
		return false
	}
	for _, b := range []byte(name) {
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-') {
			return false
		}
	}
	return true
}

func machineHomes() string {
	if runtime.GOOS == "darwin" {
		return "/Users"
	}
	return "/home"
}

type machineUser struct {
	name     string
	uid, gid int
}

func hostMachineUser() machineUser {
	name := os.Getenv("USER")
	if os.Getuid() == 0 {
		name = "root"
	} else if name == "" {
		name = "user"
	}
	return machineUser{name, os.Getuid(), os.Getgid()}
}

func (u machineUser) home() string {
	if u.name == "root" {
		return "/root"
	}
	return filepath.Join(machineHomes(), u.name)
}

func machineCommand(args []string) error {
	op, err := parseMachine(args)
	if err != nil {
		return err
	}
	in, err := locate()
	if err != nil {
		return err
	}
	probe := socketClient(in.socket())
	running := alive(probe)
	probe.CloseIdleConnections()
	if !running {
		return errors.New("fabo daemon is not running: open Fabo.app or run `fabo daemon`")
	}
	if op.verb == "shell" {
		cwd, _ := os.Getwd()
		_, ttyErr := unix.IoctlGetTermios(int(os.Stdin.Fd()), terminalGetAttr)
		return in.runDocker(machineShellArgs(op, hostMachineUser(), cwd, ttyErr == nil))
	}
	client := newCommandClient(in.socket())
	defer client.http.CloseIdleConnections()
	return runMachine(client, op, hostMachineUser(), os.Stdout)
}

func runMachine(client *commandClient, op machineOptions, user machineUser, out io.Writer) error {
	container := "machine-" + op.name
	switch op.verb {
	case "create":
		return createMachine(client, op, user, out)
	case "list":
		filters, _ := json.Marshal(map[string][]string{"label": {machineLabel}})
		var rows []struct {
			Labels        map[string]string
			Image, Status string
		}
		if err := client.request("GET", "/containers/json?all=1&filters="+url.QueryEscape(string(filters)), nil, &rows); err != nil {
			return err
		}
		fmt.Fprintf(out, "%-16s %-28s %s\n", "NAME", "IMAGE", "STATE")
		for _, row := range rows {
			fmt.Fprintf(out, "%-16s %-28s %s\n", row.Labels[machineLabel], row.Image, row.Status)
		}
		return nil
	case "delete":
		if err := client.request("DELETE", "/containers/"+container+"?force=1&v=1", nil, nil); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "deleted machine %s\n", op.name)
		return err
	default:
		return machineAction(client, container, op.verb)
	}
}

// Docker returns 304 for an already started/stopped container.
func machineAction(client *commandClient, container, verb string) error {
	err := client.request("POST", "/containers/"+container+"/"+verb, nil, nil)
	var apiErr *commandError
	if errors.As(err, &apiErr) && apiErr.status == 304 && (verb == "start" || verb == "stop") {
		return nil
	}
	return err
}

func createMachine(client *commandClient, op machineOptions, user machineUser, out io.Writer) error {
	// USER becomes both a passwd name and a path in the shared homes tree.
	if user.name == "" || strings.ContainsAny(user.name, "/:\n\r\x00") || user.name == "." || user.name == ".." || strings.HasPrefix(user.name, "-") {
		return fmt.Errorf("invalid host user name %q", user.name)
	}
	image, query := machineImage(op.image)
	fmt.Fprintf(out, "pulling %s…\n", image)
	var pulled []byte
	if err := client.request("POST", "/images/create?"+query.Encode(), nil, &pulled); err != nil {
		return err
	}
	// Pull failures can arrive in a successful HTTP stream.
	decoder := json.NewDecoder(strings.NewReader(string(pulled)))
	for {
		var step struct{ Error string }
		if err := decoder.Decode(&step); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("invalid pull response: %w", err)
		}
		if step.Error != "" {
			return errors.New(step.Error)
		}
	}
	container := "machine-" + op.name
	if err := client.request("POST", "/containers/create?name="+container, machineSpec(op, image, user), nil); err != nil {
		return err
	}
	if err := machineAction(client, container, "start"); err != nil {
		return err
	}
	// The first command creates the user; an immediate shell must not race it.
	wait := "for i in $(seq 300); do id -u " + quoteMachineWord(user.name) + " >/dev/null 2>&1 && exit 0; sleep 0.1; done; exit 1"
	code, _, err := client.exec(container, []string{"sh", "-c", wait})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("machine %s is running, but the user %s could not be added to it", op.name, user.name)
	}
	_, err = fmt.Fprintf(out, "machine %s (%s) is running: fabo machine shell %s\n", op.name, image, op.name)
	return err
}

// A registry port is not an image tag. Digests pass through unchanged.
func machineImage(image string) (string, url.Values) {
	if strings.Contains(image, "@") {
		return image, url.Values{"fromImage": {image}}
	}
	repo, tag := image, "latest"
	if n := strings.LastIndex(image, ":"); n > strings.LastIndex(image, "/") {
		repo, tag = image[:n], image[n+1:]
	}
	return repo + ":" + tag, url.Values{"fromImage": {repo}, "tag": {tag}}
}

func machineSpec(op machineOptions, image string, user machineUser) map[string]any {
	host := map[string]any{
		"Binds":         []string{machineHomes() + ":" + machineHomes()},
		"RestartPolicy": map[string]string{"Name": "unless-stopped"},
		"CapAdd":        []string{"NET_ADMIN", "SYS_PTRACE"},
	}
	body := map[string]any{
		"Image": image, "Cmd": []string{"/bin/sh", "-c", machineInit(user, op.systemd)},
		"Hostname": op.name, "Labels": map[string]string{machineLabel: op.name}, "HostConfig": host,
	}
	if op.systemd {
		body["StopSignal"] = "SIGRTMIN+3"
		body["Env"] = []string{"container=docker"}
		host["Privileged"] = true
		host["Tmpfs"] = map[string]string{"/run": "", "/run/lock": "", "/tmp": ""}
	}
	return body
}

// Values from the host environment stay shell words, never shell syntax.
func quoteMachineWord(word string) string {
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

func machineInit(user machineUser, systemd bool) string {
	script := fmt.Sprintf(`set -e
u=%s
h=%s
if ! id -u "$u" >/dev/null 2>&1; then
  getent group %[3]d >/dev/null 2>&1 || groupadd -g %[3]d "$u" 2>/dev/null || addgroup -g %[3]d "$u" 2>/dev/null || true
  g=$(getent group %[3]d | cut -d: -f1)
  old=$(getent passwd %[4]d | cut -d: -f1)
  { [ -n "$old" ] && usermod -l "$u" -d "$h" "$old" 2>/dev/null; } || useradd -o -M -u %[4]d -g "${g:-%[3]d}" -d "$h" -s /bin/sh "$u" 2>/dev/null || adduser -D -H -u %[4]d -G "${g:-$u}" -h "$h" "$u" 2>/dev/null || true
fi
mkdir -p /etc/sudoers.d && printf '%%s ALL=(ALL) NOPASSWD:ALL\n' "$u" > /etc/sudoers.d/fabo && chmod 440 /etc/sudoers.d/fabo
# Slim images have no sudo: fetch it once, in the background.
command -v sudo >/dev/null 2>&1 || (apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq sudo || apk add -q sudo || dnf -y -q install sudo || pacman -Sy --noconfirm sudo || zypper -n -q in sudo) >/dev/null 2>&1 &
`, quoteMachineWord(user.name), quoteMachineWord(user.home()), user.gid, user.uid)
	if systemd {
		return script + `wait
for i in /lib/systemd/systemd /usr/lib/systemd/systemd; do [ -x $i ] && exec $i; done
(apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq systemd systemd-sysv dbus || dnf -y -q install systemd || pacman -Sy --noconfirm systemd || zypper -n -q in systemd) >/dev/null 2>&1
for i in /lib/systemd/systemd /usr/lib/systemd/systemd; do [ -x $i ] && exec $i; done
echo 'no systemd for this distro' >&2; exit 1`
	}
	return script + "trap 'exit 0' TERM INT\nwhile :; do sleep 3600 & wait $!; done"
}

func machineShellArgs(op machineOptions, user machineUser, cwd string, tty bool) []string {
	home := user.home()
	if rel, err := filepath.Rel(machineHomes(), cwd); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		cwd = home
	}
	terminal := "-i"
	if tty {
		terminal = "-it"
	}
	args := []string{"exec", terminal, "-u", user.name, "-w", cwd, "-e", "HOME=" + home, "machine-" + op.name}
	if len(op.command) > 0 {
		return append(args, op.command...)
	}
	return append(args, "sh", "-c", "exec $(command -v bash || command -v sh) -l")
}
