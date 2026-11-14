//go:build linux || darwin

package main

// fabo debug opens a shell with debugging tools beside any running
// container, even one with no shell of its own (distroless, scratch,
// read-only). The shell runs in a toolbox container that shares the
// target's PID and network namespaces: ps and htop list the target's
// processes, strace attaches to them, curl reaches its ports on localhost,
// and its files are at /target. Tools added with dctl go into the toolbox,
// never into the target, and are kept for every later session.

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	debugUsage = "usage: fabo debug CONTAINER"
	// debugToolbox is the image the sessions run; its tag changes when the
	// tools or scripts below do, so an old toolbox is rebuilt.
	debugToolbox = "fabo/debug-toolbox:1"
	// debugVolume keeps the packages dctl installs, for every session.
	debugVolume = "fabo-debug"
	debugLabel  = "dev.fabo.debug"
	debugBase   = "alpine:3.22"
)

// debugPackages are the tools every session starts with.
var debugPackages = strings.Fields(`fish bash bash-completion coreutils findutils grep sed less ncurses
procps-ng htop curl wget ca-certificates strace lsof iproute2 iputils bind-tools net-tools
tcpdump nano vim jq file util-linux tzdata`)

// debugFiles are written into the toolbox: the session's entry, dctl and
// fish's configuration.
var debugFiles = map[string]string{
	"usr/local/bin/fabo-debug-shell": `#!/bin/sh
# Starts a debug session: restores the packages dctl installed earlier,
# points /target at the debugged container's root, and opens fish there.
cache=/var/cache/fabo-debug
mkdir -p "$cache/apk"
rm -rf /etc/apk/cache && ln -s "$cache/apk" /etc/apk/cache
if [ -s "$cache/packages" ]; then
  apk add -q $(cat "$cache/packages") >/dev/null 2>&1 || echo "fabo debug: some packages installed with dctl could not be restored" >&2
fi
ln -sfn /proc/1/root /target
cd "/target${FABO_DEBUG_WORKDIR:-/}" 2>/dev/null || cd /target
if command -v fish >/dev/null 2>&1; then exec fish -l; fi
exec bash -l
`,
	"usr/local/bin/dctl": `#!/bin/sh
# dctl adds tools to the debug shell. They go into the toolbox, never into
# the debugged container, and come back in every later session.
list=/var/cache/fabo-debug/packages
usage() { echo "usage: dctl install COMMAND... | remove PACKAGE... | list | search WORD" >&2; exit 2; }
# provider names the package that has a command, or the word itself.
provider() {
  p=$(apk search -x -q "cmd:$1" 2>/dev/null | head -n 1)
  [ -n "$p" ] || { apk update -q >/dev/null 2>&1; p=$(apk search -x -q "cmd:$1" 2>/dev/null | head -n 1); }
  echo "${p:-$1}"
}
case "$1" in
install|add)
  shift; [ $# -gt 0 ] || usage
  for word; do
    pkg=$(provider "$word")
    apk add -q "$pkg" 2>/dev/null || { apk update -q && apk add -q "$pkg"; } || { echo "dctl: no package for $word" >&2; exit 1; }
    grep -qx "$pkg" "$list" 2>/dev/null || echo "$pkg" >> "$list"
    echo "installed $pkg"
  done ;;
remove|uninstall|rm)
  shift; [ $# -gt 0 ] || usage
  for pkg; do apk del -q "$pkg" && sed -i "/^$pkg\$/d" "$list" 2>/dev/null; done ;;
list|ls) cat "$list" 2>/dev/null ;;
search) shift; [ $# -gt 0 ] || usage; apk search "$@" ;;
*) usage ;;
esac
`,
	"etc/fish/conf.d/fabo-debug.fish": `# The debug shell's prompt, greeting and the offer to install a missing command.
set -g fish_greeting (set_color --bold)"Debug shell for $FABO_DEBUG_TARGET"(set_color normal)"
  Its files are at /target; ps and htop show its processes.
  Add tools with dctl install COMMAND (the container is not changed)."
function fish_prompt
    set_color ff8700; echo -n debug; set_color normal; echo -n ' '
    set_color 87d7ff; echo -n $FABO_DEBUG_TARGET; set_color normal
    echo -n ':'(prompt_pwd)'> '
end
function fish_command_not_found
    set -l pkg (apk search -x -q "cmd:$argv[1]" 2>/dev/null | head -n 1)
    if test -z "$pkg"
        echo "fabo debug: command not found: $argv[1]" >&2
        return 127
    end
    read -l -P "$argv[1] is in the package $pkg. Install it with dctl? [Y/n] " answer
    if test -z "$answer"; or string match -qi 'y*' -- $answer
        dctl install $argv[1]; and $argv
        return $status
    end
    return 127
end
alias ll 'ls -lah'
`,
}

// debugCommand parses `fabo debug` and replaces this process with the
// Docker CLI running the session, so the terminal is Docker's.
func debugCommand(args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New(debugUsage)
	}
	in, err := locate()
	if err != nil {
		return err
	}
	client, err := in.runningClient()
	if err != nil {
		return err
	}
	defer client.http.CloseIdleConnections()
	var target struct {
		ID, Name string
		State    struct{ Running, Paused bool }
		Config   struct{ WorkingDir string }
	}
	if err := client.request("GET", "/containers/"+url.PathEscape(args[0])+"/json", nil, &target); err != nil {
		return err
	}
	name := strings.TrimPrefix(target.Name, "/")
	if !target.State.Running || target.State.Paused {
		return fmt.Errorf("%s is not running; start it to debug it", name)
	}
	if err := ensureDebugToolbox(client, os.Stderr); err != nil {
		return err
	}
	_, ttyErr := unix.IoctlGetTermios(int(os.Stdin.Fd()), terminalGetAttr)
	return in.runDocker(debugRunArgs(target.ID, name, target.Config.WorkingDir, ttyErr == nil))
}

// debugRunArgs is the docker run that holds one session: the toolbox in the
// target's PID and network namespaces, removed when the shell exits.
func debugRunArgs(id, name, workdir string, tty bool) []string {
	terminal := "-i"
	if tty {
		terminal = "-it"
	}
	short := id[:min(12, len(id))]
	return []string{"run", "--rm", terminal,
		"--name", fmt.Sprintf("fabo-debug-%s-%d", short, time.Now().UnixNano()%1_000_000),
		"--label", debugLabel + "=" + name,
		"--pid", "container:" + id, "--network", "container:" + id,
		// Tracing and entering the target's processes, and looking at its
		// network, need these; the target itself gains nothing.
		"--cap-add", "SYS_PTRACE", "--cap-add", "SYS_ADMIN", "--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW",
		"-v", debugVolume + ":/var/cache/fabo-debug",
		"-e", "FABO_DEBUG_TARGET=" + name, "-e", "FABO_DEBUG_WORKDIR=" + workdir,
		"--entrypoint", "/usr/local/bin/fabo-debug-shell", debugToolbox,
	}
}

// ensureDebugToolbox builds the toolbox image once: alpine with the tools
// installed and the session scripts added, committed under debugToolbox.
func ensureDebugToolbox(c *commandClient, log io.Writer) (err error) {
	if c.request("GET", "/images/"+debugToolbox+"/json", nil, nil) == nil {
		return nil
	}
	base, err := debugBaseImage(c, log)
	if err != nil {
		return err
	}
	fmt.Fprintln(log, "Preparing the debug tools (only the first time)…")
	builder := fmt.Sprintf("fabo-debug-build-%d", time.Now().UnixNano())
	body := map[string]any{
		"Image": base, "Entrypoint": []string{"/bin/sh", "-c"},
		"Cmd":    []string{"apk update -q && apk add -q " + strings.Join(debugPackages, " ")},
		"Labels": map[string]string{debugLabel: "toolbox-build"},
	}
	if err := c.request("POST", "/containers/create?name="+builder, body, nil); err != nil {
		return fmt.Errorf("debug toolbox: %w", err)
	}
	defer c.request("DELETE", "/containers/"+builder+"?force=1", nil, nil)
	if err := c.request("POST", "/containers/"+builder+"/start", nil, nil); err != nil {
		return fmt.Errorf("debug toolbox: %w", err)
	}
	waited, err := c.stream("POST", "/containers/"+builder+"/wait", "", nil, -1)
	if err != nil {
		return fmt.Errorf("debug toolbox: %w", err)
	}
	var result struct{ StatusCode int }
	err = json.NewDecoder(waited).Decode(&result)
	waited.Close()
	if err != nil {
		return fmt.Errorf("debug toolbox: %w", err)
	}
	if result.StatusCode != 0 {
		var logs []byte
		_ = c.request("GET", "/containers/"+builder+"/logs?stdout=1&stderr=1&tail=20", nil, &logs)
		return fmt.Errorf("installing the debug tools failed (exit %d); is the network reachable?\n%s", result.StatusCode, demultiplex(logs))
	}
	archive, err := debugArchive()
	if err != nil {
		return err
	}
	upload, err := c.stream("PUT", "/containers/"+builder+"/archive?path=/", "application/x-tar", bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return fmt.Errorf("debug toolbox scripts: %w", err)
	}
	_, _ = io.Copy(io.Discard, upload)
	upload.Close()
	repo, tag, _ := strings.Cut(debugToolbox, ":")
	q := url.Values{"container": {builder}, "repo": {repo}, "tag": {tag}}
	committed, err := c.stream("POST", "/commit?"+q.Encode(), "application/json", strings.NewReader("{}"), 2)
	if err != nil {
		return fmt.Errorf("debug toolbox: %w", err)
	}
	_, _ = io.Copy(io.Discard, committed)
	committed.Close()
	return nil
}

// debugBaseImage is a local alpine, pulling one only when none is here.
func debugBaseImage(c *commandClient, log io.Writer) (string, error) {
	var images []struct{ RepoTags []string }
	if err := c.request("GET", "/images/json", nil, &images); err != nil {
		return "", err
	}
	for _, img := range images {
		for _, tag := range img.RepoTags {
			if strings.HasPrefix(familiarName(tag), "alpine:") {
				return tag, nil
			}
		}
	}
	fmt.Fprintf(log, "Pulling %s for the debug tools…\n", debugBase)
	if err := c.pullImage(debugBase); err != nil {
		return "", fmt.Errorf("pull %s for the debug tools: %w", debugBase, err)
	}
	return debugBase, nil
}

// debugArchive packs debugFiles as a tar to extract at the toolbox's root.
func debugArchive() ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, path := range []string{"usr/local/bin/fabo-debug-shell", "usr/local/bin/dctl", "etc/fish/conf.d/fabo-debug.fish"} {
		content := debugFiles[path]
		mode := int64(0o644)
		if strings.HasPrefix(path, "usr/local/bin/") {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: path, Mode: mode, Size: int64(len(content)), ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := io.WriteString(tw, content); err != nil {
			return nil, err
		}
	}
	err := tw.Close()
	return buf.Bytes(), err
}
