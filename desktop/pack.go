package main

// Packing containers into a file and importing one: the launcher's
// `fabo pack` and `fabo unpack` do the work; the window picks the file and
// reports how it went.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo"
)

// packExtension names the files `fabo pack` writes.
const packExtension = ".fabopack"

// packTimeout bounds a pack or an import; images and volumes can be large.
const packTimeout = 2 * time.Hour

// chooseFile shows the system's save (save) or open dialog for a package and
// returns the chosen path, or "" when canceled. It blocks, so it runs off
// the main thread.
func (s *desktop) chooseFile(save bool, name string) string {
	if s.chooser != nil {
		return s.chooser(save, name)
	}
	if s.window == nil {
		return ""
	}
	filters := []mygo.FileFilter{{Name: tr("Fabo 容器包", "Fabo package"), Extensions: []string{strings.TrimPrefix(packExtension, ".")}}}
	downloads := ""
	if home, err := os.UserHomeDir(); err == nil {
		downloads = filepath.Join(home, "Downloads")
	}
	if save {
		path, _ := mygo.Dialog.Save(mygo.SaveDialogOptions{
			Parent: s.window, Title: tr("打包容器", "Pack containers"), ButtonLabel: tr("打包", "Pack"),
			DefaultPath: filepath.Join(downloads, name), Filters: filters, CreateDirectories: true,
		})
		return path
	}
	paths, _ := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Parent: s.window, Title: tr("导入容器包", "Import a package"), ButtonLabel: tr("导入", "Import"),
		DefaultPath: downloads, Filters: filters,
	})
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}

// withFile asks for a file and, when one is chosen, hands it to then on the
// main thread. Without a window, as in tests, it runs inline.
func (s *desktop) withFile(save bool, name string, then func(path string)) {
	if s.window == nil {
		if path := s.chooseFile(save, name); path != "" {
			then(path)
		}
		return
	}
	go func() {
		if path := s.chooseFile(save, name); path != "" {
			s.window.Update(func() { then(path) })
		}
	}()
}

// pack writes containers into a package: a Compose project's when project
// is set, else the named ones. base names the file it suggests.
func (s *desktop) pack(names []string, project, base string) {
	if s.launcher == nil || s.working["pack"] {
		return
	}
	s.withFile(true, base+packExtension, func(path string) {
		if !strings.HasSuffix(path, packExtension) {
			path += packExtension
		}
		args := []string{"pack", "-o", path}
		if project != "" {
			args = append(args, "--project", project)
		} else {
			args = append(args, names...)
		}
		s.working["pack"] = true
		s.run(packTimeout, func(ctx context.Context) func() {
			_, err := s.launcher(ctx, args...)
			return func() {
				delete(s.working, "pack")
				s.settle(err, fmt.Sprintf(tr("已打包到 %s", "Packed into %s"), filepath.Base(path)))
			}
		})
	})
}

// unpackDone matches the launcher's last line after an import.
var unpackDone = regexp.MustCompile(`unpacked (\d+) containers`)

// unpack imports a package chosen by the user and starts the containers that
// were running when it was made.
func (s *desktop) unpack() {
	if s.launcher == nil || s.working["unpack"] {
		return
	}
	s.withFile(false, "", func(path string) {
		s.working["unpack"] = true
		s.run(packTimeout, func(ctx context.Context) func() {
			out, err := s.launcher(ctx, "unpack", "--start", path)
			return func() {
				delete(s.working, "unpack")
				// Binds this machine lacks and containers that would not
				// start are worth telling; the import itself succeeded.
				for _, line := range strings.Split(string(out), "\n") {
					if strings.Contains(line, "not on this machine") || strings.Contains(line, "not started") {
						s.notices = append(s.notices, line)
					}
				}
				done := tr("已导入", "Imported")
				if m := unpackDone.FindStringSubmatch(string(out)); m != nil {
					n, _ := strconv.Atoi(m[1])
					done = fmt.Sprintf(tr("已导入 %d 个容器", "Imported %d containers"), n)
				}
				s.settle(err, done)
			}
		})
	})
}

// packName suggests a package's file name for the given containers.
func packName(list []container) string {
	switch {
	case len(list) == 1:
		return list[0].Name
	case len(list) > 1 && list[0].Project != "":
		for _, x := range list {
			if x.Project != list[0].Project {
				return "containers"
			}
		}
		return list[0].Project
	}
	return "containers"
}

// containerNames lists the names of containers, for the launcher.
func containerNames(list []container) []string {
	names := make([]string, len(list))
	for i, x := range list {
		names[i] = x.Name
	}
	return names
}
