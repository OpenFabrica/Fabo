// kiri-go-init runs as a VM init or as a Linux host process.
package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/libracoder/kiri/engine/internal/guestagent"
	"github.com/libracoder/kiri/engine/internal/guestwire"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "__kiri-copy" {
		guestagent.CopyMain()
		return
	}
	if len(os.Args) == 2 && (os.Args[1] == "__kiri-container" || os.Args[1] == "__kiri-exec") {
		guestagent.ContainerMain(os.Args[1] == "__kiri-exec")
		return
	}
	if os.Getpid() == 1 && os.Getenv("KIRI_AGENT_DIR") == "" {
		if err := runVM(); err != nil {
			log.Print(err)
		}
		unix.Sync()
		if err := unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	unix.Umask(0o022)
	volumeRoot := os.Getenv("KIRI_GO_VOLUME_ROOT")
	if volumeRoot != "" {
		// Receive new host mounts without propagating the agent's own
		// volumes back to the daemon's filesystem namespace.
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
			return fmt.Errorf("isolate local volume mounts: %w", err)
		}
	}
	started, err := guestagent.BoottimeNS()
	if err != nil {
		return err
	}
	dir := os.Getenv("KIRI_AGENT_DIR")
	if dir == "" {
		return fmt.Errorf("KIRI_AGENT_DIR is required for host-process mode")
	}
	// Daemon launches keep layers and writable container roots with its
	// metadata. The socket directory is removed when the agent exits.
	stateDir := os.Getenv("KIRI_GO_STATE_DIR")
	if stateDir == "" {
		stateDir = dir
	}
	layerDir := os.Getenv("KIRI_GO_LAYER_DIR")
	if layerDir == "" {
		layerDir = filepath.Join(stateDir, "layers")
	}
	path := filepath.Join(dir, fmt.Sprintf("h%d.sock", guestwire.Port))
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	containerDir := os.Getenv("KIRI_GO_CONTAINER_DIR")
	if containerDir == "" {
		containerDir = filepath.Join(stateDir, "containers")
	}
	return serve(conn, started, layerDir, containerDir, os.Getenv("KIRI_GO_CGROUP_DIR"), nil, volumeRoot, false)
}

func serve(conn io.ReadWriteCloser, started uint64, layerDir, containerDir, cgroupDir string, clock func(time.Time) error, volumeRoot string, rosetta bool) error {
	layers, err := guestagent.OpenLayers(layerDir)
	if err != nil {
		return err
	}
	processes, err := guestagent.OpenRuntime(containerDir, cgroupDir, layers, clock != nil, volumeRoot, os.Getenv("KIRI_GO_PROXY_SOCKET"))
	if err != nil {
		return err
	}
	stopArchive, err := startArchiveService(processes, clock != nil)
	if err != nil {
		return err
	}
	defer stopArchive()
	return guestagent.Serve(conn, started, layers, processes, clock, rosetta)
}
