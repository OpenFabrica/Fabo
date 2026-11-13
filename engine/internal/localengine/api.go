//go:build linux || darwin

package localengine

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

const apiVersion = "1.47"

func (e *Engine) version() string {
	if e.release != "" {
		return e.release
	}
	return "kiri-go"
}

var versionPrefix = regexp.MustCompile(`^/v[0-9]+\.[0-9]+/`)

// APIPath removes the Docker API version prefix used by clients.
func APIPath(path string) string {
	if prefix := versionPrefix.FindString(path); prefix != "" {
		return "/" + strings.TrimPrefix(path, prefix)
	}
	return path
}

func (e *Engine) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", apiVersion)
		w.Header().Set("Builder-Version", "1")
		w.Header().Set("OSType", "linux")
		fmt.Fprint(w, "OK")
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"Platform": map[string]string{"Name": "Kiri"},
			"Components": []map[string]any{{"Name": "Engine", "Version": e.version(), "Details": map[string]string{
				"ApiVersion": apiVersion, "MinAPIVersion": "1.24", "Arch": runtime.GOARCH, "Os": "linux",
			}}},
			"Version": e.version(), "KiriEngine": "go", "ApiVersion": apiVersion, "MinAPIVersion": "1.24",
			"Os": "linux", "Arch": runtime.GOARCH, "GoVersion": runtime.Version(),
		})
	})
	mux.HandleFunc("GET /info", e.infoHTTP)
	mux.HandleFunc("GET /system/df", e.diskUsageHTTP)
	mux.HandleFunc("GET /events", e.eventsHTTP)
	mux.HandleFunc("POST /build", e.buildHTTP)
	mux.HandleFunc("POST /build/cancel", e.cancelBuildHTTP)
	mux.HandleFunc("POST /build/prune", e.pruneBuildHTTP)
	mux.HandleFunc("GET /kiri/builder", e.builderSettingsHTTP)
	mux.HandleFunc("POST /kiri/builder", e.builderSettingsHTTP)
	mux.HandleFunc("POST /commit", e.commitHTTP)
	mux.HandleFunc("POST /auth", e.registryLoginHTTP)
	mux.HandleFunc("GET /networks", e.networksHTTP)
	mux.HandleFunc("GET /networks/{id}", e.inspectNetworkHTTP)
	mux.HandleFunc("POST /networks/create", e.createNetworkHTTP)
	mux.HandleFunc("POST /networks/prune", e.pruneNetworksHTTP)
	mux.HandleFunc("POST /networks/{id}/connect", e.connectNetworkHTTP)
	mux.HandleFunc("POST /networks/{id}/disconnect", e.disconnectNetworkHTTP)
	mux.HandleFunc("DELETE /networks/{id}", e.removeNetworkHTTP)
	mux.HandleFunc("GET /images/{ref...}", e.inspectImageHTTP)
	mux.HandleFunc("GET /images/get", e.saveImagesHTTP)
	mux.HandleFunc("POST /images/create", e.pullHTTP)
	mux.HandleFunc("POST /images/load", e.loadImageHTTP)
	mux.HandleFunc("POST /images/prune", e.pruneImagesHTTP)
	mux.HandleFunc("POST /images/{ref...}", e.imageActionHTTP)
	mux.HandleFunc("DELETE /images/{ref...}", e.removeImageHTTP)
	mux.HandleFunc("GET /images/json", e.imagesHTTP)
	mux.HandleFunc("GET /kiri/images/layers", e.imageLayersHTTP)
	mux.HandleFunc("POST /kiri/images/scan", e.imageScanHTTP)
	mux.HandleFunc("GET /kiri/images/scan", e.imageScanHTTP)
	mux.HandleFunc("GET /kiri/images/scans", e.imageScansHTTP)
	mux.HandleFunc("GET /kiri/images/sbom", e.imageSBOMHTTP)
	mux.HandleFunc("POST /volumes/create", e.createVolumeHTTP)
	mux.HandleFunc("POST /volumes/prune", e.pruneVolumesHTTP)
	mux.HandleFunc("GET /volumes", e.listVolumesHTTP)
	mux.HandleFunc("GET /volumes/{name}", e.inspectVolumeHTTP)
	mux.HandleFunc("DELETE /volumes/{name}", e.removeVolumeHTTP)
	mux.HandleFunc("POST /containers/create", e.createHTTP)
	mux.HandleFunc("POST /containers/prune", e.pruneContainersHTTP)
	mux.HandleFunc("POST /containers/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		if err := e.start(r.PathValue("id")); err != nil {
			if errors.Is(err, errAlreadyRunning) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			writeError(w, err)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /containers/{id}/wait", e.waitHTTP)
	mux.HandleFunc("POST /containers/{id}/kill", e.killHTTP)
	mux.HandleFunc("POST /containers/{id}/stop", e.stopHTTP)
	mux.HandleFunc("POST /containers/{id}/restart", e.restartHTTP)
	mux.HandleFunc("POST /kiri/containers/{id}/env", e.setEnvironmentHTTP)
	mux.HandleFunc("POST /kiri/containers/{id}/ports", e.setPortsHTTP)
	mux.HandleFunc("GET /kiri/containers/{id}/listening", e.listeningHTTP)
	mux.HandleFunc("POST /kiri/k8s/forward", e.forwardKubernetesHTTP)
	mux.HandleFunc("DELETE /kiri/k8s/forward/{port}", e.unforwardKubernetesHTTP)
	mux.HandleFunc("POST /containers/{id}/rename", e.renameHTTP)
	mux.HandleFunc("POST /containers/{id}/update", e.updateHTTP)
	mux.HandleFunc("POST /containers/{id}/pause", func(w http.ResponseWriter, r *http.Request) { e.pauseHTTP(w, r, true) })
	mux.HandleFunc("POST /containers/{id}/unpause", func(w http.ResponseWriter, r *http.Request) { e.pauseHTTP(w, r, false) })
	mux.HandleFunc("DELETE /containers/{id}", e.removeHTTP)
	mux.HandleFunc("POST /containers/{id}/attach", e.attachHTTP)
	mux.HandleFunc("POST /containers/{id}/resize", e.resizeHTTP)
	mux.HandleFunc("POST /containers/{id}/exec", e.createExecHTTP)
	mux.HandleFunc("POST /exec/{id}/start", e.startExecHTTP)
	mux.HandleFunc("GET /exec/{id}/json", e.inspectExecHTTP)
	mux.HandleFunc("POST /exec/{id}/resize", e.resizeExecHTTP)
	mux.HandleFunc("GET /containers/json", e.listHTTP)
	mux.HandleFunc("GET /containers/{id}/json", e.inspectHTTP)
	mux.HandleFunc("GET /containers/{id}/logs", e.logsHTTP)
	mux.HandleFunc("GET /containers/{id}/archive", e.archiveHTTP)
	mux.HandleFunc("PUT /containers/{id}/archive", e.archiveHTTP)
	mux.HandleFunc("GET /containers/{id}/export", e.exportHTTP)
	mux.HandleFunc("GET /containers/{id}/changes", e.diffHTTP)
	mux.HandleFunc("GET /containers/{id}/stats", e.statsHTTP)
	mux.HandleFunc("GET /containers/{id}/top", e.topHTTP)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = APIPath(r.URL.Path)
		mux.ServeHTTP(w, r)
	})
}

func (e *Engine) createHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Config
		MacAddress             string
		HostConfig             map[string]json.RawMessage
		AttachStdin, OpenStdin bool
		NetworkingConfig       struct {
			EndpointsConfig map[string]map[string]json.RawMessage
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, fmt.Errorf("%w: %v", errInvalid, err))
		return
	}
	if builder := e.managedBuilderName(r.URL.Query().Get("name")); builder != "" {
		id, err := e.createManagedBuilder(r.Context(), builder)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"Id": id, "Warnings": []string{}})
		return
	}
	body.Config.openStdin = body.AttachStdin || body.OpenStdin
	body.Config.platform = r.URL.Query().Get("platform")
	if raw := body.HostConfig["Privileged"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &body.Config.privileged); err != nil {
			writeError(w, fmt.Errorf("%w: invalid Privileged setting", errInvalid))
			return
		}
		delete(body.HostConfig, "Privileged")
	}
	if raw := body.HostConfig["Init"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &body.Config.init); err != nil {
			writeError(w, fmt.Errorf("%w: invalid Init setting", errInvalid))
			return
		}
		delete(body.HostConfig, "Init")
	}
	var mode string
	if raw := body.HostConfig["NetworkMode"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &mode); err != nil {
			writeError(w, fmt.Errorf("%w: invalid network mode", errInvalid))
			return
		}
	}
	if err := configureEndpoint(&body.Config, mode, body.NetworkingConfig.EndpointsConfig); err != nil {
		writeError(w, err)
		return
	}
	if body.MacAddress != "" {
		if !body.Config.bridge {
			writeError(w, fmt.Errorf("%w: conflicting options: mac-address and the network mode", errInvalid))
			return
		}
		mac, err := guestwire.ParseEndpointMAC(body.MacAddress)
		if err != nil {
			writeError(w, fmt.Errorf("%w: %v", errInvalid, err))
			return
		}
		body.Config.networkMAC = mac
	}
	extraHosts, err := parseExtraHosts(body.HostConfig["ExtraHosts"])
	if err != nil {
		writeError(w, err)
		return
	}
	body.Config.extraHosts = extraHosts
	delete(body.HostConfig, "ExtraHosts")
	if raw := body.HostConfig["Links"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &body.Config.links); err != nil {
			writeError(w, fmt.Errorf("%w: invalid Links list", errInvalid))
			return
		}
		delete(body.HostConfig, "Links")
	}
	body.Config.dns, err = parseDNSConfig(body.HostConfig)
	if err != nil {
		writeError(w, err)
		return
	}
	body.Config.securityOpt, err = parseSecurityOptions(body.HostConfig["SecurityOpt"])
	if err != nil {
		writeError(w, err)
		return
	}
	delete(body.HostConfig, "SecurityOpt")
	body.Config.capabilities, err = parseCapabilities(body.HostConfig)
	if err != nil {
		writeError(w, err)
		return
	}
	body.Config.process, err = parseProcessOptions(body.HostConfig)
	if err != nil {
		writeError(w, err)
		return
	}
	body.Config.devices, err = parseDeviceOptions(body.HostConfig)
	if err != nil {
		writeError(w, err)
		return
	}
	body.Config.sysctls, err = parseSysctlConfig(body.HostConfig)
	if err != nil {
		writeError(w, err)
		return
	}
	bindings, err := parsePortBindings(body.HostConfig["PortBindings"])
	if err != nil {
		writeError(w, err)
		return
	}
	body.Config.portBindings = bindings
	if raw := body.HostConfig["PublishAllPorts"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &body.Config.publishAll); err != nil {
			writeError(w, fmt.Errorf("%w: invalid PublishAllPorts", errInvalid))
			return
		}
	}
	limits, err := parseLimits(body.HostConfig)
	if err != nil {
		writeError(w, err)
		return
	}
	body.Config.limits = limits
	body.Config.restart, err = parseRestartPolicy(body.HostConfig["RestartPolicy"])
	if err != nil {
		writeError(w, err)
		return
	}
	delete(body.HostConfig, "RestartPolicy")
	binds, volumes, tmpfs, readOnly, err := parseMounts(body.HostConfig, e.windowsMounts)
	if err != nil {
		writeError(w, err)
		return
	}
	body.Config.mounts, body.Config.volumes, body.Config.readOnlyRoot = binds, volumes, readOnly
	body.Config.tmpfs = tmpfs
	for field, value := range body.HostConfig {
		if field == "NetworkMode" || field == "PortBindings" || field == "PublishAllPorts" {
			continue
		}
		switch string(bytes.TrimSpace(value)) {
		case "null", "false", "0", `""`, "[]", "{}":
			continue
		}
		// Docker emits zero-valued structured defaults for these fields.
		switch field {
		case "AutoRemove":
			if string(value) == "true" {
				body.Config.autoRemove = true
				continue
			}
		case "MemorySwappiness":
			if string(value) == "-1" {
				continue
			}
		case "ConsoleSize":
			var size []uint16
			if err := json.Unmarshal(value, &size); err != nil || len(size) != 2 {
				writeError(w, fmt.Errorf("%w: invalid ConsoleSize", errInvalid))
				return
			}
			copy(body.Config.consoleSize[:], size)
			continue
		case "LogConfig":
			var logging struct {
				Type   string
				Config map[string]string
			}
			if json.Unmarshal(value, &logging) == nil && (logging.Type == "" || logging.Type == "json-file") && len(logging.Config) == 0 {
				continue
			}
		}
		writeError(w, fmt.Errorf("%w: unsupported HostConfig option %s", errInvalid, field))
		return
	}
	id, err := e.create(r.URL.Query().Get("name"), body.Config)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"Id": id, "Warnings": []string{}})
}

func (e *Engine) listHTTP(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filters, err := parseFilters(query.Get("filters"), "label", "label!", "name", "status", "id", "health", "ancestor", "network", "exited", "volume")
	if err != nil {
		writeError(w, err)
		return
	}
	for _, value := range filters["health"] {
		switch value {
		case "none", healthStarting, healthHealthy, healthUnhealthy:
		default:
			writeError(w, fmt.Errorf("%w: unrecognised health filter %q", errInvalid, value))
			return
		}
	}
	all := flagSet(query, "all") || len(filters["status"]) > 0 || len(filters["exited"]) > 0
	e.mu.Lock()
	matched := make([]*container, 0, len(e.containers))
	for _, c := range e.containers {
		if (!all && c.state != "running" && c.restartTimer == nil) || !e.matchesContainer(c, filters) {
			continue
		}
		matched = append(matched, c)
	}
	slices.SortFunc(matched, func(a, b *container) int {
		return cmp.Or(cmp.Compare(b.created, a.created), strings.Compare(a.id, b.id))
	})
	rows := make([]map[string]any, 0, len(matched))
	for _, c := range matched {
		rows = append(rows, e.containerSummary(c))
	}
	e.mu.Unlock()
	writeJSON(w, 200, rows)
}

// containerSummary is called with the engine state lock held.
func (e *Engine) containerSummary(c *container) map[string]any {
	return map[string]any{
		"Id": c.id, "Names": []string{"/" + c.name}, "Image": c.image, "ImageID": c.imageID, "Created": c.created,
		"Command": strings.Join(c.run.Argv, " "), "State": c.dockerState(), "Status": c.listStatus(),
		"KiriStartedAt": unixOrNil(c.startedAt), "KiriFinishedAt": unixOrNil(c.finishedAt),
		"KiriExitCode": c.exit, "KiriOOMKilled": c.oomKilled, "KiriVmStopped": c.vmStopped,
		"HostConfig":      map[string]string{"NetworkMode": c.networkMode()},
		"NetworkSettings": e.networkSettings(c), "Ports": c.listedPorts(), "Labels": c.labels, "Mounts": e.containerMounts(c),
	}
}

func unixOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

func (c *container) listStatus() string {
	switch c.dockerState() {
	case "created":
		return "Created"
	case "exited":
		status := fmt.Sprintf("Exited (%d)", c.exit)
		if !c.finishedAt.IsZero() {
			status += " " + formatUptime(time.Since(c.finishedAt)) + " ago"
		}
		return status
	case "restarting":
		return fmt.Sprintf("Restarting (%d)", c.exit)
	case "running", "paused":
		uptime := "Up"
		if c.attempt != nil && !c.attempt.startedAt.IsZero() {
			uptime += " " + formatUptime(time.Since(c.attempt.startedAt))
		}
		switch {
		case c.paused:
			return uptime + " (Paused)"
		case c.health == nil:
			return uptime
		case c.health.Status == healthStarting:
			return uptime + " (health: starting)"
		default:
			return uptime + " (" + c.health.Status + ")"
		}
	default:
		return c.state
	}
}

func formatUptime(age time.Duration) string {
	switch {
	case age < 2*time.Second:
		return "Less than a second"
	case age < time.Minute:
		return fmt.Sprintf("%d seconds", age/time.Second)
	case age < time.Hour:
		return fmt.Sprintf("%d minutes", age/time.Minute)
	case age < 24*time.Hour:
		return fmt.Sprintf("%d hours", age/time.Hour)
	default:
		return fmt.Sprintf("%d days", age/(24*time.Hour))
	}
}

func (e *Engine) inspectHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	c, err := e.find(r.PathValue("id"))
	if err != nil {
		e.mu.Unlock()
		writeError(w, err)
		return
	}
	imageID := c.imageID
	if imageID == "" {
		imageID = c.image
	}
	hostConfig := map[string]any{
		"AutoRemove": c.run.Flags&guestwire.RunAutoRemove != 0, "NetworkMode": c.networkMode(),
		"Privileged":   c.run.Flags&guestwire.RunPrivileged != 0,
		"PortBindings": dockerBindings(c.portBindings, nil), "RestartPolicy": c.restartPolicy.inspect(),
		"Dns": c.run.DNS.Servers, "DnsSearch": c.run.DNS.Search, "DnsOptions": c.run.DNS.Options,
		"GroupAdd": c.run.Process.Groups, "Ulimits": append([]guestwire.Ulimit{}, c.run.Process.Ulimits...),
		"Sysctls": c.run.Sysctls,
		"Links":   c.inspectLinks(""),
		"Devices": c.run.Devices.Mappings, "DeviceCgroupRules": c.run.Devices.Rules,
		"CapAdd": c.capabilities.Add, "CapDrop": c.capabilities.Drop, "SecurityOpt": c.securityOpt,
	}
	inspectLimits(hostConfig, c.run.Limits)
	tmpfs := make(map[string]string)
	for _, value := range c.run.Mounts {
		if guestwire.IsTmpfsMount(value) {
			if mount, err := guestwire.DecodeTmpfsMount(value); err == nil {
				tmpfs[mount.Target] = strings.Join(mount.Options, ",")
			}
		}
	}
	if len(tmpfs) != 0 {
		hostConfig["Tmpfs"] = tmpfs
	}
	if c.init != nil {
		hostConfig["Init"] = *c.init
	}
	if entries := c.run.Network.ExtraHosts; len(entries) != 0 {
		hosts := make([]string, 0, len(entries))
		for _, entry := range entries {
			if address, hostname, err := guestwire.ParseExtraHost(entry); err == nil {
				hosts = append(hosts, hostname+":"+address)
			}
		}
		hostConfig["ExtraHosts"] = hosts
	}
	var pid uint32
	if c.state == "running" && c.attempt != nil {
		pid = c.attempt.pid
	}
	path, args := "", []string{}
	if len(c.run.Argv) > 0 {
		path = c.run.Argv[0]
		args = append(args, c.run.Argv[1:]...)
	}
	body := map[string]any{
		"Id": c.id, "Name": "/" + c.name, "Image": imageID, "RestartCount": c.restartCount,
		"Created": time.Unix(c.created, 0).UTC().Format(time.RFC3339),
		"Path":    path, "Args": args,
		"Config": map[string]any{
			"Image": c.image, "Tty": c.run.Flags&guestwire.RunTTY != 0, "ExposedPorts": c.exposedPorts, "Labels": c.labels,
			"Env": c.run.Env, "Hostname": c.run.Hostname, "Domainname": c.run.Domainname, "WorkingDir": c.run.Cwd,
			"OpenStdin": c.run.Flags&guestwire.RunStdin != 0, "AttachStdin": c.run.Flags&guestwire.RunStdin != 0,
		},
		"HostConfig":      hostConfig,
		"NetworkSettings": e.networkSettings(c),
		"State": map[string]any{
			"Status": c.dockerState(), "Running": c.state == "running", "Paused": c.paused,
			"Restarting": c.restartTimer != nil, "Pid": pid, "ExitCode": c.exit, "OOMKilled": c.oomKilled,
			"StartedAt": c.startedAt.UTC().Format(time.RFC3339Nano), "FinishedAt": c.finishedAt.UTC().Format(time.RFC3339Nano),
		},
	}
	if c.healthcheck != nil {
		body["Config"].(map[string]any)["Healthcheck"] = c.healthcheck
	}
	if c.health != nil {
		health := *c.health
		health.Log = slices.Clone(health.Log)
		body["State"].(map[string]any)["Health"] = health
	}
	body["Mounts"] = e.containerMounts(c)
	e.mu.Unlock()
	writeJSON(w, 200, body)
}

func (e *Engine) containerMounts(c *container) []map[string]any {
	mounts := make([]map[string]any, 0, len(c.run.Mounts))
	for _, value := range c.run.Mounts {
		if guestwire.IsTmpfsMount(value) {
			if m, err := guestwire.DecodeTmpfsMount(value); err == nil {
				mounts = append(mounts, map[string]any{"Type": "tmpfs", "Source": "", "Destination": m.Target, "RW": !m.ReadOnly(), "Propagation": ""})
			}
			continue
		}
		if m, err := guestwire.DecodeBindMount(value); err == nil {
			entry := map[string]any{"Type": "bind", "Source": m.Source, "Destination": m.Target, "RW": !m.ReadOnly, "Propagation": "rprivate"}
			if name := e.mountVolumeName(c, m); name != "" {
				entry["Type"], entry["Name"], entry["Driver"], entry["Propagation"] = "volume", name, "local", ""
			}
			mounts = append(mounts, entry)
		}
	}
	return mounts
}

func writeError(w http.ResponseWriter, err error) {
	status := 500
	switch {
	case errors.Is(err, errInvalid):
		status = 400
	case errors.Is(err, errMissing):
		status = 404
	case errors.Is(err, errConflict):
		status = 409
	}
	writeJSON(w, status, map[string]string{"message": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
