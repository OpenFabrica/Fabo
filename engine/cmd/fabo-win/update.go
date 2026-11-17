package main

// updateView is GET /update: the window's update card.
type updateView struct {
	// State is "unavailable" until the updater is configured.
	State string `json:"state"`
}

// updater checks for, downloads and installs new versions.
type updater struct{ h *host }

// newUpdater returns the host's updater.
func newUpdater(h *host) *updater { return &updater{h: h} }

// installStagedAtLaunch installs an update staged before; true when this
// process should exit for it.
func (u *updater) installStagedAtLaunch() bool { return false }

// installStagedAtQuit installs a staged update as fabo-win quits.
func (u *updater) installStagedAtQuit() {}

// loop checks for updates in the background.
func (u *updater) loop() {}

// state is GET /update.
func (u *updater) state() updateView { return updateView{State: "unavailable"} }

// action is POST /update/{action}.
func (u *updater) action(string) error { return errUnknownAction }
