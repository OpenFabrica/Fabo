package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"

	"github.com/OpenFabrica/Fabo/engine/internal/hostagent"
	"github.com/OpenFabrica/Fabo/engine/internal/localengine"
)

type guestOptions struct {
	agent        string
	systemd      bool
	wsl          bool
	windowsProxy bool
	proxySocket  string
}

func (o *guestOptions) register(flags *flag.FlagSet) {
	flags.StringVar(&o.agent, "agent", "", "Fabo agent executable")
	flags.BoolVar(&o.systemd, "systemd", false, "use the service's delegated cgroup")
	flags.BoolVar(&o.wsl, "wsl", false, "use Fabo's cgroup on a WSL host without systemd")
	flags.BoolVar(&o.windowsProxy, "windows-proxy", false, "accept proxy configuration from the Windows host")
}

func (o *guestOptions) configureEngineSocket(socket string) bool {
	if o.windowsProxy {
		o.proxySocket = socket
	}
	return o.windowsProxy
}

func (o guestOptions) valid() bool { return o.agent != "" }

func (o guestOptions) prepare(context.Context) error { return nil }

func (o guestOptions) startFinder(*supervisor) func() { return func() {} }

func (o guestOptions) start(ctx context.Context, stateDir string) (guestConnection, error) {
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return guestConnection{}, err
	}
	env := []string{
		"FABO_STATE_DIR=" + filepath.Join(stateDir, "guest"),
		"FABO_VOLUME_ROOT=" + filepath.Join(stateDir, "volumes"),
		"FABO_PROXY_SOCKET=" + o.proxySocket,
	}
	cgroupDir := os.Getenv("FABO_CGROUP_DIR")
	if cgroupDir == "" && (o.systemd || o.wsl) {
		if o.systemd {
			cgroupDir, err = prepareServiceCgroup()
		} else {
			cgroupDir, err = prepareWSLCgroup()
		}
		if err != nil {
			return guestConnection{}, err
		}
		env = append(env, "FABO_CGROUP_DIR="+cgroupDir)
	}
	p, err := hostagent.StartInMountNamespace(ctx, o.agent, os.Stderr, env...)
	if err != nil {
		return guestConnection{}, err
	}
	return guestConnection{conn: p.Conn, close: p.Close, archive: p.Dial, cgroups: cgroupDir != ""}, nil
}

func (o guestOptions) vmConfiguration() *vmConfiguration { return nil }

func (o guestOptions) withVMSettings(_ localengine.VMSettings) guestOptions { return o }
