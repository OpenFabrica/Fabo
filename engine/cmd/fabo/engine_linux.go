package main

import "errors"

// restartEngine is the system service's on Linux, which the desktop cannot
// restart without privileges.
func (i installation) restartEngine() error {
	return errors.New("restart the system service to use the installed engine: sudo systemctl restart fabo")
}
