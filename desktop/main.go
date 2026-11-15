package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

//go:embed tray.png
var trayIcon []byte

const (
	overviewPage   = "overview"
	containersPage = "containers"
	imagesPage     = "images"
	volumesPage    = "volumes"
	networksPage   = "networks"
	buildsPage     = "builds"
	settingsPage   = "settings"
)

// navItem is one entry of the sidebar. The soon entries hold a place for
// features the window does not have yet.
type navItem struct {
	page, icon, zh, en string
	soon               bool
}

var navItems = []navItem{
	{overviewPage, "grid", "概览", "Overview", false},
	{containersPage, "box", "容器", "Containers", false},
	{imagesPage, "layers", "镜像", "Images", false},
	{volumesPage, "cylinder", "卷", "Volumes", false},
	{networksPage, "network", "网络", "Networks", false},
	{buildsPage, "hammer", "构建", "Builds", false},
	{machinesPage, "cpu", "Linux 机器", "Machines", false},
	{kubernetesPage, "helm", "Kubernetes", "Kubernetes", false},
	{"usb", "usb", "USB 设备", "USB devices", true},
	{"share", "share", "共享", "Share", true},
	{terminalPage, "terminal", "终端", "Terminal", false},
	{settingsPage, "sliders", "设置", "Settings", false},
}

// pageTitle is the page's name in the window's language.
func pageTitle(page string) string {
	for _, n := range navItems {
		if n.page == page {
			return tr(n.zh, n.en)
		}
	}
	return page
}

// engineAction is one request that changes the engine.
type engineAction struct {
	method, path string
	body         any
}

// confirmation is a question the user must answer before run happens.
type confirmation struct {
	title, body, ok string
	run             func()
}

// form holds the fields of whichever dialog is open; kind names the dialog.
type form struct {
	kind                           string
	image, name, host, port, value string
	problem                        string
	// named is set when the dialog opens with its image already chosen,
	// so that typing starts at the name. It is fixed for the dialog's
	// life: deciding it from the field's content would move the focus as
	// soon as the first letter was typed.
	named bool
	// flag is the dialog's one switch, as New machine's systemd.
	flag bool
}

// detail is what the dock shows for the selected row. key ties it to that
// row, so an answer that arrives after the selection moved is dropped.
type detail struct {
	key      string
	fields   []field
	layers   []layer
	files    []fileEntry
	filesFor string
	filesErr string
	preview  filePreview
}

type desktop struct {
	window *mygo.Window
	engine engineClient
	// launcher runs this installation's `fabo` command, which makes the
	// machine-level changes the engine cannot. Nil when there is none.
	launcher func(ctx context.Context, args ...string) ([]byte, error)
	// chooser replaces the system's file dialogs in tests.
	chooser func(save bool, name string) string
	// launcherPath is the fabo command launcher runs, for terminal tabs
	// that run one of its commands; openTerminal replaces those tabs in
	// tests.
	launcherPath string
	openTerminal func(terminal.Options)
	// machines are the Linux machines, kept out of containers.
	machines []container
	// machineTerm names the machine whose terminal page shows; each
	// machine's shells are its tabs, by terminal key, with the chosen one.
	machineTerm   string
	machineTabs   map[string][]string
	machineActive map[string]string
	ctx           context.Context
	prefs         prefs
	prefsFile     string
	locale        string
	dark          bool
	contextName   string

	page, search string
	searchBox    ui.Handle

	// What the engine last reported.
	connected  bool
	problem    string
	info       engineInfo
	res        vmResources
	containers []container
	images     []image
	volumes    []volume
	networks   []network
	usage      map[string]*usage
	cpuHist    []float32
	disk       *diskUsage
	events     []engineEvent
	builder    builderInfo
	domains    domainInfo
	version    versionInfo
	scans      map[string]*scanResult
	scanning   map[string]float64
	now        int64

	// What the window shows.
	selected  map[string]string
	tab       map[string]int
	filter    map[string]int
	collapsed map[string]bool
	// picked holds the IDs of the containers ticked for a bulk action.
	picked map[string]bool
	// compose is the Compose file open in a project's Config tab.
	compose composeEditor
	// width is the window's width in layout units, as of the last frame;
	// navPeek unfolds the sidebar while a narrow window folds it.
	width   float32
	navPeek bool
	lists   map[string]*ui.ListState
	visible []string
	dockMax bool
	// detailPanel builds the selected row's panel; the page registers it
	// while it builds and the view places it beside the page.
	detailPanel func()
	detail      detail
	filePath    map[string]string
	logFilter   string
	logLevel    int
	follow      bool
	pending     *confirmation
	form        form
	notices     []string
	working     map[string]bool
	// changing is the start, stop or restart under way for a container,
	// by ID, so its row can say so until the engine answers.
	changing map[string]string
	// menus are the rows' more-actions menus, by button label.
	menus map[string]*moreMenuState
	// staleLater hides the strip asking to restart an engine of another
	// version, until the window is opened again.
	staleLater  bool
	refreshing  bool
	settingsTab int
	settingsFor string
	cpus        float64
	memory      float64
	// netMode is the network mode picked in Settings → Network, as an index
	// into vmNetworkModes, until it is saved.
	netMode    int
	cacheGB    string
	resOK      bool
	proxy      proxyInfo
	proxyDraft proxySettings
	proxyOK    bool
	domainsOK  bool
	// domainsRead marks that the containers page asked for the domain
	// settings once, so its rows can show their local addresses.
	domainsRead    bool
	names          nameState
	namesOK        bool
	builderOK      bool
	portHTTP       string
	portHTTPS      string
	credentials    *registryStore
	registries     registryUI
	kube           kubernetesUI
	imp            importUI
	containerDraft containerSettings
	fonts          []string
	monoFonts      []string

	terminals              map[string]*terminalSession
	hostSessions           []string
	hostActive, hostShell  string
	hostSequence           int
	containerLogs, podLogs *liveLogs
	// logsCleared holds when each container's logs were last cleared, so a
	// reopened stream starts there instead of replaying the engine's tail.
	logsCleared      map[string]time.Time
	closingTerminals []*terminalSession
	closingLogs      []*liveLogs
}

// newDesktop makes the window's state with every map ready to use.
func newDesktop(engine engineClient) *desktop {
	return &desktop{
		engine: engine, ctx: context.Background(), page: containersPage, follow: true,
		prefs: defaultPrefs(), credentials: newRegistryStore(), terminals: map[string]*terminalSession{},
		usage: map[string]*usage{}, scans: map[string]*scanResult{}, scanning: map[string]float64{},
		selected: map[string]string{}, tab: map[string]int{containersPage: 1}, filter: map[string]int{containersPage: 1},
		collapsed: map[string]bool{}, picked: map[string]bool{}, lists: map[string]*ui.ListState{},
		filePath: map[string]string{}, working: map[string]bool{}, changing: map[string]string{}, menus: map[string]*moreMenuState{}, now: time.Now().Unix(),
		machineTabs: map[string][]string{}, machineActive: map[string]string{},
		logsCleared: map[string]time.Time{},
	}
}

// release is this window's version, stamped from the git tag by
// scripts/macos-app.sh like the launcher's and the engine's. A plain
// `go build` or `go run` keeps the default, which compares with nothing.
var release = "0.0.0-dev"

func main() {
	initial := flag.String("view", containersPage, "initial page")
	wake := flag.Bool("start", false, "start the Fabo engine")
	flag.Parse()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	s := newDesktop(newEngineClient(dialEngine))
	s.ctx, s.launcher = ctx, findLauncher()
	if s.launcher != nil {
		s.launcherPath = launcherBesideExecutable()
	}
	for _, n := range navItems {
		if strings.EqualFold(n.page, *initial) {
			s.page = n.page
		}
	}
	// The docker context is named after the state directory: ~/.fabo-dev
	// holds the context fabo-dev.
	s.contextName = "fabo"
	if socket := os.Getenv("FABO_SOCKET"); socket != "" {
		s.contextName = strings.TrimPrefix(filepath.Base(filepath.Dir(filepath.Dir(socket))), ".")
	}
	tags := make([]string, 0, len(languages))
	for _, language := range languages {
		tags = append(tags, language.tag)
	}
	mygo.App.SetLanguages(tags...)
	mygo.App.WhenReady(func() {
		s.prefsFile = prefsPath()
		s.prefs = loadPrefs(s.prefsFile)
		s.logsCleared = loadClearedLogs(clearedLogsPath(s.prefsFile))
		s.locale = systemLanguage(mygo.App.PreferredLanguages())
		s.applyLanguage()
		mygo.App.OnLocaleChanged(func(string) {
			s.locale = systemLanguage(mygo.App.PreferredLanguages())
			s.invalidate()
		})
		s.applyTheme()
		s.window = mygo.NewWindow(mygo.WindowOptions{
			Title: "Fabo", Width: 1180, Height: 760, MinWidth: 860, MinHeight: 520,
			TitleBarStyle: mygo.TitleBarHidden, TitleBarHeight: stripHeight,
			TrafficLightPosition: &mygo.Point{X: 14, Y: 13},
			BackgroundColor:      "light-dark(#F4F6F9, #0E1115)", StateKey: "main",
			Content: ui.View(s.view),
		})
		s.installTray()
		s.window.OnClosed(func() { mygo.App.Quit() })
		mygo.App.OnQuit(func() { cancel(); s.closeInteractive() })
		go func() {
			select {
			case <-signals:
				mygo.App.Quit()
			case <-ctx.Done():
			}
		}()
		if *wake {
			s.do("engine", engineAction{http.MethodPost, "/fabo/wake", nil}, "")
		} else {
			s.refresh()
		}
		// The lists follow the engine while the window is open. These reads
		// are background reads, so they never keep an idle VM awake.
		go func() {
			for range time.Tick(2 * time.Second) {
				mygo.RunOnMain(s.refresh)
			}
		}()
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

// findLauncher locates the `fabo` command of this installation: the one
// FABO_LAUNCHER names, else the file beside this executable, as in the app
// bundle. It returns nil when neither exists, as under `go run`.
func findLauncher() func(ctx context.Context, args ...string) ([]byte, error) {
	path := launcherBesideExecutable()
	if path == "" {
		return nil
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil
	}
	return func(ctx context.Context, args ...string) ([]byte, error) {
		out, err := exec.CommandContext(ctx, path, args...).Output()
		// The launcher explains a failure on stderr as "fabo: <reason>".
		var failed *exec.ExitError
		if errors.As(err, &failed) {
			if reason := strings.TrimPrefix(strings.TrimSpace(string(failed.Stderr)), "fabo: "); reason != "" {
				return out, errors.New(reason)
			}
		}
		return out, err
	}
}

// applyTheme makes the window chrome follow the chosen appearance.
func (s *desktop) applyTheme() {
	source := mygo.ThemeSystem
	switch s.prefs.Theme {
	case "light":
		source = mygo.ThemeLight
	case "dark":
		source = mygo.ThemeDark
	}
	mygo.Theme.SetSource(source)
}

// savePrefs keeps a changed setting and, with a window, applies the theme.
func (s *desktop) savePrefs() {
	s.prefs.save(s.prefsFile)
	if s.window != nil {
		s.applyTheme()
	}
}

func (s *desktop) installTray() {
	openItem := &mygo.MenuItem{Label: "Open Fabo", Click: func(*mygo.MenuItem, *mygo.Window) {
		s.window.Show()
		s.refresh()
	}}
	update := func(string) { openItem.SetLabel(textFor(s.prefs.language(s.locale), "打开 Fabo", "Open Fabo")) }
	update("")
	mygo.App.OnLocaleChanged(update)
	menu := mygo.NewMenu([]*mygo.MenuItem{openItem,
		mygo.Separator(),
		{Role: mygo.RoleQuit},
	})
	_, err := mygo.NewTray(mygo.TrayOptions{
		ID: "fabo", Icon: trayIcon, IconIsTemplate: true, ToolTip: "Fabo", Menu: menu,
	})
	if err != nil {
		// Linux desktops without an indicator can use the window directly.
		log.Printf("tray unavailable: %v", err)
		return
	}
	quitting := false
	mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) { quitting = true })
	s.window.OnClose(func(e *mygo.CloseEvent) {
		if !quitting {
			e.PreventDefault()
			s.window.Hide()
		}
	})
	mygo.App.OnActivate(func(visible bool) {
		if !visible {
			s.window.Show()
		}
	})
}

// run does work off the main thread and applies its result on it. work must
// not touch the window's state: it returns a function that does. Without a
// window, as in tests, the work runs inline so its effect is immediate.
func (s *desktop) run(timeout time.Duration, work func(ctx context.Context) func()) {
	if s.engine.http == nil {
		return
	}
	if s.window == nil {
		ctx, cancel := context.WithTimeout(s.ctx, timeout)
		defer cancel()
		if apply := work(ctx); apply != nil {
			apply()
		}
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(s.ctx, timeout)
		defer cancel()
		apply := work(ctx)
		s.window.Update(func() {
			if apply != nil {
				apply()
			}
		})
	}()
}

// awake reports whether the engine can answer live questions such as usage.
// An engine running directly on Linux has no VM and is always awake.
func (i engineInfo) awake() bool { return !i.FaboStopped && (i.FaboVmAwake == nil || *i.FaboVmAwake) }

// engineState names the engine's condition for the sidebar and the title bar.
func (s *desktop) engineState() string {
	switch {
	case s.working["engine-stop"]:
		return "stopping"
	case s.working["engine"]:
		return "starting"
	case !s.connected:
		return "offline"
	case s.info.FaboStopped:
		return "stopped"
	case !s.info.awake():
		return "asleep"
	}
	return "running"
}

// refresh reads what the current page shows. One read runs at a time; a tick
// that finds the last one still going is skipped.
func (s *desktop) refresh() {
	if s.refreshing || (s.window != nil && !s.window.IsVisible()) {
		return
	}
	s.refreshing = true
	if s.page == kubernetesPage || s.page == settingsPage {
		s.refreshKubernetes()
	}
	page := s.page
	wantDisk := page == overviewPage && s.disk == nil
	s.run(8*time.Second, func(ctx context.Context) func() {
		e := s.engine
		info, err := e.info(ctx)
		if err != nil {
			return func() { s.refreshing, s.connected, s.problem = false, false, err.Error() }
		}
		containers, e1 := e.containers(ctx)
		images, e2 := e.images(ctx)
		volumes, e3 := e.volumes(ctx)
		networks, e4 := e.networks(ctx)
		var samples map[string]sample
		if info.awake() && (page == containersPage || page == overviewPage) {
			var ids []string
			for _, c := range containers {
				if c.State == "running" {
					ids = append(ids, c.ID)
				}
			}
			samples = e.samples(ctx, ids)
		}
		var res vmResources
		resErr := e.get(ctx, "/fabo/resources", &res)
		now := time.Now().Unix()
		var events []engineEvent
		var disk *diskUsage
		var builder builderInfo
		var builderErr, eventsErr error
		switch page {
		case overviewPage:
			events, eventsErr = e.events(ctx, now)
			// Measuring the disk boots a sleeping VM, so it is read only
			// once per visit and only while the VM is already up.
			if wantDisk && info.awake() {
				disk, _ = e.diskUsage(ctx)
			}
		case buildsPage:
			builderErr = e.get(ctx, "/fabo/builder", &builder)
		}
		return func() {
			s.refreshing, s.connected, s.problem, s.info, s.now = false, true, "", info, now
			if e1 == nil {
				s.containers, s.machines = splitContainers(containers)
				s.reconcileTerminals()
			}
			if e2 == nil {
				s.images = images
			}
			if e3 == nil {
				s.volumes = volumes
			}
			if e4 == nil {
				s.networks = networks
			}
			if resErr == nil {
				s.res = res
			}
			if eventsErr == nil && page == overviewPage {
				s.events = events
			}
			if disk != nil {
				s.disk = disk
			}
			if builderErr == nil && page == buildsPage {
				s.builder = builder
			}

			if samples != nil {
				s.fold(samples)
			}
		}
	})
	if s.engine.http == nil {
		s.refreshing = false
	}
}

// fold applies a round of samples: each running container's usage advances,
// the ones that stopped are forgotten, and the engine's total joins the
// history the overview charts.
func (s *desktop) fold(samples map[string]sample) {
	total := 0.0
	for id, sm := range samples {
		u := s.usage[id]
		if u == nil {
			u = &usage{}
			s.usage[id] = u
		}
		u.advance(sm)
		total += u.cpu
	}
	for id := range s.usage {
		if _, ok := samples[id]; !ok {
			delete(s.usage, id)
		}
	}
	s.cpuHist = append(s.cpuHist, float32(total/float64(s.cores())))
	if len(s.cpuHist) > 90 {
		s.cpuHist = s.cpuHist[len(s.cpuHist)-90:]
	}
}

// cores is the number of CPUs the engine runs on, at least one.
func (s *desktop) cores() int {
	return max(1, s.info.NCPU, s.res.RunningCPUs)
}

// memoryUse returns the memory the running containers hold and the engine's
// total, in bytes. The total is zero when the engine does not report one.
func (s *desktop) memoryUse() (used, total int64) {
	for _, u := range s.usage {
		used += int64(u.memory)
	}
	total = s.info.MemTotal
	if total == 0 {
		total = int64(s.res.RunningMemory) << 20
	}
	return used, total
}

// do sends one change to the engine. key marks the row as working until the
// answer comes; done is announced when it succeeds.
func (s *desktop) do(key string, a engineAction, done string) {
	s.working[key] = true
	s.run(2*time.Minute, func(ctx context.Context) func() {
		err := s.engine.request(ctx, a.method, a.path, a.body, nil)
		return func() {
			delete(s.working, key)
			s.settle(err, done)
		}
	})
	if s.engine.http == nil {
		delete(s.working, key)
	}
}

// settle reports how a change ended and reads the engine again.
func (s *desktop) settle(err error, done string) {
	if err != nil {
		s.notices = append(s.notices, err.Error())
	} else if done != "" {
		s.notices = append(s.notices, done)
	}
	// What changed may be something Settings shows.
	s.settingsFor = ""
	s.refreshing = false
	s.refresh()
}

// navigate shows another page. The search belongs to the page it was typed on.
func (s *desktop) navigate(page string) {
	if page == s.page {
		return
	}
	s.page, s.search, s.dockMax = page, "", false
	s.settingsFor = ""
	if page == overviewPage {
		s.disk = nil
	}
	s.refreshing = false
	s.refresh()
}

// selectRow opens the dock on a row of the current page; "" closes it.
func (s *desktop) selectRow(id string) {
	s.selected[s.page] = id
	if id == "" {
		s.dockMax = false
	}
}

// open jumps to a row of another page, as the overview and the "used by"
// lists do.
func (s *desktop) open(page, id string) {
	s.navigate(page)
	s.selected[page] = id
}

// containerPath is the API path of a container.
func containerPath(id string) string { return "/containers/" + url.PathEscape(id) }

func (s *desktop) startContainer(c container)   { s.changeContainers([]container{c}, "start") }
func (s *desktop) stopContainer(c container)    { s.changeContainers([]container{c}, "stop") }
func (s *desktop) restartContainer(c container) { s.changeContainers([]container{c}, "restart") }

// changeContainers starts, stops or restarts containers together. Their rows
// say what is under way, and one notice reports the outcome: the container's
// name for one, a count for several, and each failure on its own.
func (s *desktop) changeContainers(list []container, action string) {
	if len(list) == 0 {
		return
	}
	for _, x := range list {
		s.working[x.ID], s.changing[x.ID] = true, action
	}
	s.run(2*time.Minute, func(ctx context.Context) func() {
		errs := make([]error, len(list))
		var wg sync.WaitGroup
		for i, x := range list {
			wg.Go(func() {
				errs[i] = s.engine.request(ctx, http.MethodPost, containerPath(x.ID)+"/"+action, nil, nil)
			})
		}
		wg.Wait()
		return func() {
			ok := 0
			for i, x := range list {
				delete(s.working, x.ID)
				delete(s.changing, x.ID)
				if errs[i] != nil {
					s.notices = append(s.notices, x.Name+tr("：", ": ")+errs[i].Error())
				} else {
					ok++
				}
			}
			done := ""
			if ok == 1 && len(list) == 1 {
				done = changedText(action, 0, list[0].Name)
			} else if ok > 0 {
				done = changedText(action, ok, "")
			}
			s.settle(nil, done)
		}
	})
	if s.engine.http == nil {
		for _, x := range list {
			delete(s.working, x.ID)
			delete(s.changing, x.ID)
		}
	}
}

// changedText words a finished change: "Restarted web" for one named
// container, or "Restarted 3 containers" when n counts several.
func changedText(action string, n int, name string) string {
	switch action {
	case "start":
		if n > 0 {
			return fmt.Sprintf(tr("已启动 %d 个容器", "Started %d containers"), n)
		}
		return fmt.Sprintf(tr("已启动 %s", "Started %s"), name)
	case "stop":
		if n > 0 {
			return fmt.Sprintf(tr("已停止 %d 个容器", "Stopped %d containers"), n)
		}
		return fmt.Sprintf(tr("已停止 %s", "Stopped %s"), name)
	}
	if n > 0 {
		return fmt.Sprintf(tr("已重启 %d 个容器", "Restarted %d containers"), n)
	}
	return fmt.Sprintf(tr("已重启 %s", "Restarted %s"), name)
}

// containerStatus is a container's status cell: the change under way while
// one runs, else its state.
func (s *desktop) containerStatus(c *ui.Context, p palette, x container) ui.Element {
	switch s.changing[x.ID] {
	case "start":
		return label(c, tr("启动中…", "Starting…"), stateColor(p, "busy"))
	case "stop":
		return label(c, tr("停止中…", "Stopping…"), stateColor(p, "busy"))
	case "restart":
		return label(c, tr("重启中…", "Restarting…"), stateColor(p, "busy"))
	}
	return label(c, x.status(s.now), stateColor(p, x.bucket()))
}

// removeContainer asks first. A running container is removed by force.
func (s *desktop) removeContainer(c container) {
	q := &confirmation{title: tr("删除容器 ", "Delete container ") + c.Name + tr("？", "?"), ok: tr("删除", "Delete")}
	path := containerPath(c.ID) + "?v=0"
	if c.bucket() == "running" {
		q.body = tr("容器正在运行，会被强制停止。", "The container is running and will be force-stopped.")
		path += "&force=1"
	}
	q.run = func() {
		s.do(c.ID, engineAction{http.MethodDelete, path, nil}, tr("已删除 ", "Deleted ")+c.Name)
	}
	s.pending = q
}

// loadDetail reads what the dock shows for the selected row beyond the list.
func (s *desktop) loadDetail(key string) {
	page, id, _ := strings.Cut(key, "/")
	now := s.now
	switch page {
	case containersPage:
		c, ok := s.container(id)
		if !ok {
			return
		}
		s.run(8*time.Second, func(ctx context.Context) func() {
			fields, _ := s.engine.containerFields(ctx, c, now)
			return func() {
				if s.detail.key == key {
					s.detail.fields = fields
				}
			}
		})
	case imagesPage:
		img, ok := s.image(id)
		if !ok {
			return
		}
		s.run(15*time.Second, func(ctx context.Context) func() {
			fields, _ := s.engine.imageFields(ctx, img, now)
			layers, _ := s.engine.layers(ctx, img.ref())
			scan, scanErr := s.engine.lastScan(ctx, img.ref())
			return func() {
				if scanErr == nil {
					s.scans[img.ID] = scan
				}
				if s.detail.key == key {
					s.detail.fields, s.detail.layers = fields, layers
				}
			}
		})
	}
}

// container finds a listed container by ID.
func (s *desktop) container(id string) (container, bool) {
	for _, c := range s.containers {
		if c.ID == id {
			return c, true
		}
	}
	return container{}, false
}

// image finds a listed image by ID.
func (s *desktop) image(id string) (image, bool) {
	for _, i := range s.images {
		if i.ID == id {
			return i, true
		}
	}
	return image{}, false
}

// imageUsers lists the containers made from an image.
func (s *desktop) imageUsers(img image) []container {
	var out []container
	for _, c := range s.containers {
		if c.ImageID == img.ID {
			out = append(out, c)
		}
	}
	return out
}

// volumeUsers lists the containers that mount a volume.
func (s *desktop) volumeUsers(name string) []container {
	var out []container
	for _, c := range s.containers {
		for _, m := range c.Mounts {
			if m.Name == name {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// networkUsers lists the containers attached to a network.
func (s *desktop) networkUsers(name string) []container {
	var out []container
	for _, c := range s.containers {
		for _, n := range c.Networks {
			if n.Network == name {
				out = append(out, c)
				break
			}
		}
	}
	return out
}
