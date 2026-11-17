package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/winhost"
)

// First run on Windows: from a PC with no WSL to `docker run` working.
// Steps: WSL (downloaded with progress, installed after one administrator
// approval, then perhaps a restart), a Linux for the engine (Fabo Linux
// from the install folder, or Ubuntu), the engine, the Docker tools,
// ready. Progress lives in setup.json, so a restart resumes. Upstream
// Kiri's crates/kiri-win/src/setup.rs; the window draws it from setupView.

// pin is a file setup downloads, with the SHA-256 it must have.
type pin struct {
	Name   string
	URL    string
	SHA256 string
	Size   int64
	File   string
}

// GitHub's asset digests for WSL's release, one MSI per architecture.
var wslMSI = map[string]pin{
	"amd64": {"WSL 3.0.3", "https://github.com/microsoft/WSL/releases/download/3.0.3/wsl.3.0.3.0.x64.msi",
		"ae977286974255bd26bf925f00f40f5692c6b2084155cbc0563c09aea091f211", 367_575_040, "wsl.3.0.3.0.x64.msi"},
	"arm64": {"WSL 3.0.3", "https://github.com/microsoft/WSL/releases/download/3.0.3/wsl.3.0.3.0.arm64.msi",
		"2937c7ce840bb35ec6e97f052189bdfff27f8c3463bfdd231df9a958212787a3", 341_086_208, "wsl.3.0.3.0.arm64.msi"},
}

// Ubuntu's WSL images, with the hashes from Microsoft's distribution list
// (DistributionInfo.json in microsoft/WSL).
var ubuntuImage = map[string]pin{
	"amd64": {"Ubuntu 26.04", "https://releases.ubuntu.com/26.04.1/ubuntu-26.04.1-wsl-amd64.wsl",
		"48d56724b5c8e60f24893e83e73bbb58c60b3ca22fba3da977075420acd54104", 418_495_746, "ubuntu-26.04.1-wsl-amd64.wsl"},
	"arm64": {"Ubuntu 26.04", "https://cdimages.ubuntu.com/releases/26.04.1/release/ubuntu-26.04.1-wsl-arm64.wsl",
		"c7d893d7ccb291009e6096c5a946937337b6fd4c426f11b6541f6cd2bf894f7d", 412_840_386, "ubuntu-26.04.1-wsl-arm64.wsl"},
}

// ubuntuDistro is the name a downloaded Ubuntu gets.
const ubuntuDistro = "Ubuntu-26.04"

// setupTask is one line of a step's checklist.
type setupTask struct {
	Label  string `json:"label"`
	State  string `json:"state"` // "todo", "active", "done", "error"
	Detail string `json:"detail"`
}

// setupProgress is the bar under a step.
type setupProgress struct {
	Pct   *int64 `json:"pct"`
	Done  *int64 `json:"done"`
	Total *int64 `json:"total"`
	Rate  *int64 `json:"rate"`
	ETA   *int64 `json:"eta"`
	Label string `json:"label"`
}

// setupView is GET /setup: everything the window's setup page shows.
type setupView struct {
	// Step is "welcome", "wsl", "restart", "choose", "linux", "engine",
	// "tools" or "ready"; "done" once setup has finished.
	Step           string           `json:"step"`
	Linux          string           `json:"linux,omitempty"`
	Sizes          map[string]int64 `json:"sizes"`
	ExistingUbuntu string           `json:"existingUbuntu,omitempty"`
	OtherDocker    bool             `json:"otherDocker"`
	MakeDefault    bool             `json:"makeDefault"`
	Autostart      bool             `json:"autostart"`
	Busy           bool             `json:"busy"`
	Progress       *setupProgress   `json:"progress"`
	Tasks          []setupTask      `json:"tasks"`
	Log            []string         `json:"log"`
	Error          string           `json:"error,omitempty"`
}

// setupSystem is everything setup does to the PC, so the steps can be
// tested without Windows.
type setupSystem interface {
	arch() string
	wslInstalled() bool
	distros() []string
	bootTime() time.Time
	engineAlive() bool
	otherDocker() bool
	// installWSL runs fabo-admin.ps1 -Wsl elevated; its exit code.
	installWSL(msi, sha256 string) (uint32, error)
	// resumeAfterRestart makes Fabo open once after Windows restarts.
	resumeAfterRestart() error
	restartWindows() error
	importDistro(name, dir, tarball string) error
	installUbuntu(file, dir string) error
	// installEngine runs install.sh as root in a distro.
	installEngine(distro string) error
	startFaboLinux() error
	setDistro(distro string)
	ensureEngine() error
	// testContainer runs `docker run --rm alpine echo hello`; its output.
	testContainer() (string, error)
	// runTools runs setup-tools.ps1, reporting each finished task.
	runTools(makeDefault bool, done func(task int)) error
	setAutostart(on bool) error
	verified(rel string) (string, error)
	logf(format string, args ...any)
}

// errBusy is an action refused while a step is running.
var errBusy = errors.New("setup is busy")

// setup runs first-run setup.
type setup struct {
	sys  setupSystem
	dir  string
	http *http.Client

	mu     sync.Mutex
	view   setupView
	busy   bool
	cancel context.CancelFunc
	// finished is called once when setup completes.
	finished func()
}

// newSetup starts setup at the step this PC is at.
func newSetup(sys setupSystem, dir string) *setup {
	s := &setup{sys: sys, dir: dir, http: &http.Client{}}
	saved := winhost.LoadSaved(dir)
	s.view = setupView{
		Sizes:       map[string]int64{"wsl": wslMSI[sys.arch()].Size, "ubuntu": ubuntuImage[sys.arch()].Size},
		MakeDefault: saved.MakeDefault == nil || *saved.MakeDefault,
		Autostart:   saved.Autostart == nil || *saved.Autostart,
		Linux:       saved.Linux,
		Error:       saved.LastError,
		Tasks:       []setupTask{},
		Log:         []string{},
	}
	s.view.Step = s.currentStep(saved)
	return s
}

// needed reports whether setup has to run: never finished, or the distro
// it made is gone.
func needed(sys setupSystem, dir string) bool {
	saved := winhost.LoadSaved(dir)
	if !saved.Done || saved.Distro == "" {
		return true
	}
	for _, d := range sys.distros() {
		if d == saved.Distro {
			return false
		}
	}
	return true
}

// currentStep is the step to show, from what is on this PC.
func (s *setup) currentStep(saved winhost.Saved) string {
	if saved.Done && !needed(s.sys, s.dir) {
		return "done"
	}
	if !s.sys.wslInstalled() {
		return "welcome"
	}
	if saved.RestartAfter != 0 && !s.sys.bootTime().After(time.Unix(saved.RestartAfter, 0)) {
		return "restart"
	}
	if saved.Distro != "" {
		for _, d := range s.sys.distros() {
			if d == saved.Distro {
				if s.sys.engineAlive() {
					return "tools"
				}
				return "engine"
			}
		}
	}
	return "choose"
}

// state is GET /setup.
func (s *setup) state() setupView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.view
	v.Busy = s.busy
	v.Tasks = append([]setupTask{}, s.view.Tasks...)
	v.Log = append([]string{}, s.view.Log...)
	if v.Step == "choose" || v.Step == "welcome" {
		v.ExistingUbuntu = s.existingUbuntu()
		v.OtherDocker = s.sys.otherDocker()
	}
	return v
}

// existingUbuntu is an Ubuntu already in WSL, which setup can use.
func (s *setup) existingUbuntu() string {
	for _, d := range s.sys.distros() {
		if strings.HasPrefix(d, "Ubuntu") {
			return d
		}
	}
	return ""
}

// update changes the view under the lock.
func (s *setup) update(f func(v *setupView)) {
	s.mu.Lock()
	f(&s.view)
	s.mu.Unlock()
}

// tasks starts a step's checklist.
func (s *setup) tasks(labels ...string) {
	s.update(func(v *setupView) {
		v.Tasks = make([]setupTask, len(labels))
		for i, l := range labels {
			v.Tasks[i] = setupTask{Label: l, State: "todo"}
		}
	})
}

// task sets one checklist line.
func (s *setup) task(i int, state, detail string) {
	s.update(func(v *setupView) {
		if i < len(v.Tasks) {
			v.Tasks[i].State, v.Tasks[i].Detail = state, detail
		}
	})
}

// logLine adds a line to the step's log (the last 200 are kept).
func (s *setup) logLine(line string) {
	s.sys.logf("setup: %s", line)
	s.update(func(v *setupView) {
		v.Log = append(v.Log, line)
		if len(v.Log) > 200 {
			v.Log = v.Log[len(v.Log)-200:]
		}
	})
}

// action is POST /setup/{action}.
func (s *setup) action(name string, value json.RawMessage) error {
	switch name {
	case "default", "autostart":
		var on bool
		if err := json.Unmarshal(value, &on); err != nil {
			return fmt.Errorf("%s needs true or false", name)
		}
		s.update(func(v *setupView) {
			if name == "default" {
				v.MakeDefault = on
			} else {
				v.Autostart = on
			}
		})
		saved := winhost.LoadSaved(s.dir)
		if name == "default" {
			saved.MakeDefault = &on
		} else {
			saved.Autostart = &on
		}
		return winhost.SaveSaved(s.dir, saved)
	case "cancel":
		s.mu.Lock()
		if s.cancel != nil {
			s.cancel()
		}
		s.mu.Unlock()
		return nil
	case "restart":
		return s.sys.restartWindows()
	case "refresh":
		if s.isBusy() {
			return nil
		}
		saved := winhost.LoadSaved(s.dir)
		s.update(func(v *setupView) { v.Step = s.currentStep(saved) })
		return nil
	case "install-wsl":
		return s.run("wsl", s.installWSL)
	case "choose":
		var kind string
		if err := json.Unmarshal(value, &kind); err != nil || (kind != "fabo" && kind != "ubuntu") {
			return errors.New(`choose needs "fabo" or "ubuntu"`)
		}
		return s.run("linux", func(ctx context.Context) error { return s.installLinux(ctx, kind) })
	case "retry":
		saved := winhost.LoadSaved(s.dir)
		step := s.currentStep(saved)
		switch step {
		case "welcome":
			return s.run("wsl", s.installWSL)
		case "engine", "tools":
			return s.run(step, func(ctx context.Context) error {
				if step == "engine" {
					return s.engine(ctx)
				}
				return s.tools(ctx)
			})
		}
		s.update(func(v *setupView) { v.Step, v.Error = step, "" })
		return nil
	case "finish":
		return s.finish()
	}
	return errUnknownAction
}

// isBusy reports whether a step is running.
func (s *setup) isBusy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}

// run starts work as step in the background; one at a time. A failure is
// kept in setup.json, so a restart of Fabo shows why setup stopped.
func (s *setup) run(step string, work func(ctx context.Context) error) error {
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		return errBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.busy, s.cancel = true, cancel
	s.view.Step, s.view.Error, s.view.Progress, s.view.Log = step, "", nil, []string{}
	s.mu.Unlock()
	go func() {
		defer cancel()
		err := work(ctx)
		saved := winhost.LoadSaved(s.dir)
		if err != nil && ctx.Err() != nil {
			err = nil // cancelled: back to the step's start, no error
			s.update(func(v *setupView) { v.Step = s.currentStep(saved) })
		}
		saved.LastError = ""
		if err != nil {
			saved.LastError = err.Error()
			s.sys.logf("setup %s: %v", step, err)
		}
		_ = winhost.SaveSaved(s.dir, saved)
		s.mu.Lock()
		s.busy, s.cancel = false, nil
		s.view.Progress = nil
		if err != nil {
			s.view.Error = err.Error()
			for i := range s.view.Tasks {
				if s.view.Tasks[i].State == "active" {
					s.view.Tasks[i].State = "error"
				}
			}
		}
		s.mu.Unlock()
	}()
	return nil
}

// mb is a byte count in the MB (10^6) a download's bar shows.
func mb(n int64) int64 { return (n + 500_000) / 1_000_000 }

// download fetches p into dir\downloads, with progress for task i, and
// checks its SHA-256. A file already there with the right hash is reused.
func (s *setup) download(ctx context.Context, p pin, i int, source string) (string, error) {
	dl := filepath.Join(s.dir, "downloads")
	if err := os.MkdirAll(dl, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(dl, p.File)
	if sum, err := winhost.FileSHA256(dest); err == nil && sum == p.SHA256 {
		s.task(i, "done", fmt.Sprintf("%d MB · already here", mb(p.Size)))
		return dest, nil
	}
	label := fmt.Sprintf("Downloading %s from %s", p.Name, source)
	s.task(i, "active", fmt.Sprintf("0 / %d MB", mb(p.Size)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading %s failed: check the internet connection (%w)", p.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s failed: %s", p.Name, resp.Status)
	}
	part := dest + ".part"
	f, err := os.Create(part)
	if err != nil {
		return "", err
	}
	defer os.Remove(part)
	h := sha256.New()
	start, last, lastDone, rate := time.Now(), time.Now(), int64(0), 0.0
	var got int64
	buf := make([]byte, 256<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if got+int64(n) > p.Size*2 {
				f.Close()
				return "", fmt.Errorf("%s is larger than expected", p.Name)
			}
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				return "", err
			}
			h.Write(buf[:n])
			got += int64(n)
		}
		if dt := time.Since(last).Seconds(); dt >= 0.25 || rerr != nil {
			now := float64(got-lastDone) / max(dt, 0.001)
			if rate == 0 {
				rate = now
			} else {
				rate = rate*0.7 + now*0.3
			}
			last, lastDone = time.Now(), got
			pct, done, total, r := got*100/max(p.Size, 1), got, p.Size, int64(rate)
			eta := int64(0)
			if rate > 0 {
				eta = int64(float64(max(p.Size-got, 0))/rate + 0.999)
			}
			s.update(func(v *setupView) {
				v.Progress = &setupProgress{Pct: &pct, Done: &done, Total: &total, Rate: &r, ETA: &eta, Label: label}
				if i < len(v.Tasks) {
					v.Tasks[i].Detail = fmt.Sprintf("%d / %d MB", mb(got), mb(p.Size))
				}
			})
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", fmt.Errorf("downloading %s failed: check the internet connection (%w)", p.Name, rerr)
		}
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != p.SHA256 {
		return "", fmt.Errorf("%s did not match its SHA-256 (got %s)", p.Name, sum)
	}
	if err := os.Rename(part, dest); err != nil {
		return "", err
	}
	s.task(i, "done", fmt.Sprintf("%d MB in %d s", mb(p.Size), int(time.Since(start).Seconds())))
	return dest, nil
}

// installWSL is step 1: download WSL, then in one elevated run turn on
// Virtual Machine Platform and install WSL. Exit 3010 means a restart.
func (s *setup) installWSL(ctx context.Context) error {
	s.tasks("Download WSL", "Administrator approval", "Turn on Virtual Machine Platform", "Install WSL")
	p, ok := wslMSI[s.sys.arch()]
	if !ok {
		return fmt.Errorf("no WSL installer for %s", s.sys.arch())
	}
	msi, err := s.download(ctx, p, 0, "Microsoft")
	if err != nil {
		return err
	}
	s.update(func(v *setupView) { v.Progress = &setupProgress{Label: "Waiting for Windows' approval"} })
	s.task(1, "active", "Windows is asking")
	// fabo-admin.ps1 checks the MSI again, as administrator, on a copy the
	// user cannot touch, and requires Microsoft's signature: the download
	// sits in a user-writable folder between this check and that run.
	code, err := s.sys.installWSL(msi, p.SHA256)
	if err != nil {
		return err
	}
	s.task(1, "done", "approved")
	switch code {
	case 22:
		return errors.New("the downloaded WSL installer changed on disk; try again")
	case 23:
		return errors.New("the WSL installer is not validly signed by Microsoft")
	case 20:
		s.task(2, "error", "")
		return errors.New("Windows could not turn on Virtual Machine Platform (is virtualization on in the BIOS?)")
	case 21:
		s.task(2, "done", "")
		s.task(3, "error", "")
		return errors.New("the WSL installer failed")
	case 0, 3010:
	default:
		return fmt.Errorf("installing WSL failed (exit code %d)", code)
	}
	s.task(2, "done", "")
	s.task(3, "done", "")
	if code == 3010 || !s.sys.wslInstalled() {
		saved := winhost.LoadSaved(s.dir)
		saved.RestartAfter = time.Now().Unix()
		if err := winhost.SaveSaved(s.dir, saved); err != nil {
			return err
		}
		// After the restart, Fabo opens once by itself and carries on.
		if err := s.sys.resumeAfterRestart(); err != nil {
			return fmt.Errorf("Windows would not register Fabo to carry on after the restart: %w", err)
		}
		s.update(func(v *setupView) { v.Step, v.Progress = "restart", nil })
		return nil
	}
	s.update(func(v *setupView) { v.Step, v.Progress = "choose", nil })
	return nil
}

// installLinux is step 2: Fabo Linux from the install folder, or Ubuntu
// (one already here, or downloaded), then the engine.
func (s *setup) installLinux(ctx context.Context, kind string) error {
	base := filepath.Join(s.dir, "wsl")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	var distro string
	if kind == "fabo" {
		s.tasks("Unpack Fabo Linux", "Add it to WSL", "Start it")
		s.task(0, "active", "")
		tarball, err := s.sys.verified("fabo-linux.tar.gz")
		if err != nil {
			return err
		}
		if info, err := os.Stat(tarball); err == nil {
			s.task(0, "done", fmt.Sprintf("%d MB", mb(info.Size())))
		}
		s.task(1, "active", "")
		if !contains(s.sys.distros(), winhost.Distro) {
			s.logLine(fmt.Sprintf("wsl --import %s %s fabo-linux.tar.gz --version 2", winhost.Distro, filepath.Join(base, winhost.Distro)))
			if err := s.sys.importDistro(winhost.Distro, filepath.Join(base, winhost.Distro), tarball); err != nil {
				return err
			}
		}
		s.task(1, "done", "")
		s.task(2, "active", "")
		// The archive carries this build's engine, and its wsl.conf already
		// says how it boots: no install.sh here (ensureEngine syncs the
		// binaries after later updates).
		if err := s.sys.startFaboLinux(); err != nil {
			return err
		}
		s.task(2, "done", "")
		distro = winhost.Distro
	} else {
		if existing := s.existingUbuntu(); existing != "" {
			s.tasks("Use the Ubuntu already here", "Install Fabo's engine in it", "Start it")
			s.task(0, "done", existing)
			distro = existing
		} else {
			s.tasks("Download Ubuntu", "Unpack it into WSL", "Install Fabo's engine in it", "Start it")
			p, ok := ubuntuImage[s.sys.arch()]
			if !ok {
				return fmt.Errorf("no Ubuntu image for %s", s.sys.arch())
			}
			file, err := s.download(ctx, p, 0, "ubuntu.com")
			if err != nil {
				return err
			}
			s.update(func(v *setupView) { v.Progress = nil })
			s.task(1, "active", "≈ 1.6 GB")
			if err := s.sys.installUbuntu(file, filepath.Join(base, ubuntuDistro)); err != nil {
				return err
			}
			s.task(1, "done", "")
			s.update(func(v *setupView) { v.Tasks = v.Tasks[1:] })
			distro = ubuntuDistro
		}
		s.task(1, "active", "")
		if err := s.sys.installEngine(distro); err != nil {
			return err
		}
		s.task(1, "done", "")
		s.task(2, "done", "")
	}
	saved := winhost.LoadSaved(s.dir)
	saved.Linux, saved.Distro = kind, distro
	if err := winhost.SaveSaved(s.dir, saved); err != nil {
		return err
	}
	s.sys.setDistro(distro)
	s.update(func(v *setupView) { v.Linux = kind })
	return s.engine(ctx)
}

// engine is step 3: the engine answers through the pipe and runs a
// container.
func (s *setup) engine(ctx context.Context) error {
	s.update(func(v *setupView) { v.Step, v.Progress = "engine", nil })
	s.tasks("Start the engine", "Connect Windows to it", "Run a test container")
	s.task(0, "active", "")
	start := time.Now()
	if err := s.sys.ensureEngine(); err != nil {
		s.sys.logf("setup engine: %v; still waiting for it", err)
	}
	for !s.sys.engineAlive() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(start) > time.Minute {
			return errors.New("Fabo's engine did not answer within a minute")
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.task(0, "done", fmt.Sprintf("answered in %d ms", time.Since(start).Milliseconds()))
	s.task(1, "done", winhost.Pipe)
	s.task(2, "active", "pulling alpine")
	s.logLine("$ docker run --rm alpine echo hello")
	out, err := s.sys.testContainer()
	lines := nonEmptyLines(out)
	for _, l := range lines[max(0, len(lines)-3):] {
		s.logLine(l)
	}
	if err != nil || !strings.Contains(out, "hello") {
		return errors.New(testFailure(out))
	}
	s.task(2, "done", "hello")
	return s.tools(ctx)
}

// testFailure says why `docker run alpine` failed, from what docker printed.
func testFailure(text string) string {
	lines := nonEmptyLines(text)
	said := ""
	for _, l := range lines {
		if _, r, ok := strings.Cut(l, "Error response from daemon:"); ok {
			said = r
			break
		}
	}
	if said == "" {
		for _, l := range lines {
			if r, ok := strings.CutPrefix(l, "docker: "); ok {
				said = r
				break
			}
		}
	}
	if said == "" {
		for i := len(lines) - 1; i >= 0; i-- {
			if !strings.HasPrefix(lines[i], "See '") && !strings.HasPrefix(lines[i], "Run '") {
				said = lines[i]
				break
			}
		}
	}
	said = strings.TrimSuffix(strings.TrimSpace(said), ".")
	pulling := strings.Contains(text, "Unable to find image") && !strings.Contains(text, "Status: Downloaded")
	switch {
	case said != "" && pulling:
		return "couldn't pull alpine: " + said
	case said != "":
		return "the test container did not run: " + said
	}
	return "the test container did not run"
}

// tools is step 4: the docker CLI, Compose, buildx and the `fabo` command
// on PATH, the `fabo` context and builder, and Fabo as docker's default if
// asked (setup-tools.ps1).
func (s *setup) tools(ctx context.Context) error {
	s.update(func(v *setupView) { v.Step, v.Progress = "tools", nil })
	s.tasks("docker", "Compose", "buildx", "docker build on Fabo", "Make Fabo the default")
	s.task(0, "active", "")
	s.mu.Lock()
	makeDefault := s.view.MakeDefault
	s.mu.Unlock()
	err := s.sys.runTools(makeDefault, func(n int) {
		detail := ""
		if n == 4 && !makeDefault {
			detail = "skipped"
		}
		s.task(n, "done", detail)
		s.task(n+1, "active", "")
	})
	if err != nil {
		return err
	}
	s.update(func(v *setupView) { v.Step, v.Progress = "ready", nil })
	return nil
}

// finish is Ready → Open Fabo: remember setup is done, and start Fabo at
// sign-in if asked.
func (s *setup) finish() error {
	s.mu.Lock()
	if s.view.Step != "ready" || s.busy {
		s.mu.Unlock()
		return errors.New("setup is not ready to finish")
	}
	autostart := s.view.Autostart
	s.mu.Unlock()
	if err := s.sys.setAutostart(autostart); err != nil {
		s.sys.logf("setup: start at sign-in: %v", err)
	}
	saved := winhost.LoadSaved(s.dir)
	saved.Done, saved.RestartAfter, saved.LastError = true, 0, ""
	if err := winhost.SaveSaved(s.dir, saved); err != nil {
		return err
	}
	s.update(func(v *setupView) { v.Step = "done" })
	if s.finished != nil {
		s.finished()
	}
	return nil
}

// contains reports whether list has v.
func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// nonEmptyLines splits text into trimmed, non-empty lines.
func nonEmptyLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
