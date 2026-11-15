package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// installedApp is the standard installation a development window borrows:
// the copy of the app the installed LaunchAgent runs, else
// /Applications/Fabo.app.
func (i installation) installedApp() (installation, error) {
	standard := namesFor(appID)
	launcher := installedLauncher(i.home, standard)
	if launcher == "" {
		return installation{}, fmt.Errorf("no installed Fabo.app to share an engine with; install it, or build with FABO_OWN_ENGINE=1 for an engine of this build's own")
	}
	resolved, err := filepath.EvalSymlinks(launcher)
	if err != nil {
		return installation{}, err
	}
	if resolved == filepath.Join(i.dir, "fabo") {
		return installation{}, fmt.Errorf("%s is this build itself, not the installed app", resolved)
	}
	return installation{dir: filepath.Dir(resolved), home: i.home, names: standard}, nil
}

// installedLauncher finds the standard installation's fabo: the program its
// LaunchAgent starts, which is the copy that owns ~/.fabo, else the one in
// /Applications. It returns "" when neither is a file.
func installedLauncher(home string, n names) string {
	var candidates []string
	agent := (&setup{in: installation{home: home, names: n}}).agentPath()
	if raw, err := os.ReadFile(agent); err == nil {
		// ProgramArguments is <string>…/fabo</string><string>daemon</string>.
		if _, rest, ok := strings.Cut(string(raw), "<key>ProgramArguments</key>"); ok {
			if _, rest, ok = strings.Cut(rest, "<string>"); ok {
				if program, _, ok := strings.Cut(rest, "</string>"); ok {
					candidates = append(candidates, strings.ReplaceAll(strings.ReplaceAll(program, "&lt;", "<"), "&amp;", "&"))
				}
			}
		}
	}
	candidates = append(candidates, "/Applications/"+n.app+".app/Contents/MacOS/fabo")
	for _, path := range candidates {
		if isFile(path) {
			return path
		}
	}
	return ""
}
