package buildwith

import (
	"errors"
	"path/filepath"
	"strings"
)

// Windows' names, as setup-tools.ps1 makes them: the `fabo` context and
// Fabo's builder on it.
const (
	WindowsBuilder = "fabo-build"
	WindowsContext = "fabo"
)

// WindowsPlaces is where fabo-win finds docker's files and keeps
// build.json: docker's config under %USERPROFILE% (or DOCKER_CONFIG), and
// build.json in Fabo's own folder (%LOCALAPPDATA%\Fabo, or FABO_WIN_DIR),
// which uninstalling removes after giving the builds back. Without a
// build tag, so it is tested everywhere.
func WindowsPlaces(getenv func(string) string, faboDir string) (Places, error) {
	home := getenv("USERPROFILE")
	if home == "" {
		return Places{}, errors.New("USERPROFILE is not set")
	}
	if faboDir == "" {
		return Places{}, errors.New("Fabo's folder is not known")
	}
	docker, buildx := DockerDirs(home, getenv)
	return Places{
		Docker: docker, Buildx: buildx, State: filepath.Join(faboDir, "build.json"),
		Builder: WindowsBuilder, Context: WindowsContext,
	}, nil
}

// WindowsOtherEngine names the engine besides Fabo on this PC, if there is
// one: installed (Docker Desktop, Rancher Desktop, Podman), or what
// fabo-win saw holding \\.\pipe\docker_engine (held, "" when Fabo serves
// it or nobody does). Upstream looked for the pipe itself, but fabo-win
// serves it whenever no other engine does, so its presence alone would
// make Fabo count as another engine and never take the builds.
func WindowsOtherEngine(getenv func(string) string, isFile func(string) bool, held string) string {
	under := func(env, rel string) bool {
		dir := getenv(env)
		return dir != "" && isFile(filepath.Join(dir, rel))
	}
	switch {
	case under("ProgramFiles", `Docker\Docker\Docker Desktop.exe`):
		return "Docker Desktop"
	case under("ProgramFiles", `Rancher Desktop\Rancher Desktop.exe`),
		under("LOCALAPPDATA", `Programs\Rancher Desktop\Rancher Desktop.exe`):
		return "Rancher Desktop"
	case under("ProgramFiles", `RedHat\Podman\podman.exe`):
		return "Podman"
	}
	return held
}

// WindowsEngineName is the engine a running program belongs to, by the
// names fabo-win watches for (host_windows.go's otherEngines).
func WindowsEngineName(process string) string {
	switch strings.ToLower(process) {
	case "":
		return ""
	case "docker desktop.exe", "com.docker.backend.exe":
		return "Docker Desktop"
	case "rancher desktop.exe":
		return "Rancher Desktop"
	}
	return "Docker"
}
