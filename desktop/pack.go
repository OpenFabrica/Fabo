package main

// Packing containers into a file and importing one: the launcher's
// `fabo pack` and `fabo unpack` do the work; the window picks the file and
// reports how it went.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// fileKind is what a file dialog is for: its title, its button and the
// files it shows.
type fileKind struct {
	title, button, filter string
	extensions            []string
}

// packFiles are container packages; imageFiles are image archives, as
// docker save writes them, compressed or not.
var (
	packFiles = func(save bool) fileKind {
		return chooseKind(save, tr("打包容器", "Pack containers"), tr("Fabo 容器包", "Fabo package"), "fabopack")
	}
	imageFiles = func(save bool) fileKind {
		return chooseKind(save, tr("打包镜像", "Pack images"), tr("镜像归档", "Image archive"), "tar", "gz", "tgz", "zst", "bz2", "xz")
	}
)

// chooseKind fills a fileKind for saving (Pack) or opening (Import).
func chooseKind(save bool, title, filter string, extensions ...string) fileKind {
	if save {
		return fileKind{title, tr("打包", "Pack"), filter, extensions[:1]}
	}
	return fileKind{tr("导入", "Import"), tr("导入", "Import"), filter, extensions}
}

// chooseFile shows the system's save (save) or open dialog and returns the
// chosen path, or "" when canceled. It blocks, so it runs off the main
// thread.
func (s *desktop) chooseFile(save bool, name string, kind fileKind) string {
	if s.chooser != nil {
		return s.chooser(save, name)
	}
	if s.window == nil {
		return ""
	}
	filters := []mygo.FileFilter{{Name: kind.filter, Extensions: kind.extensions}}
	downloads := ""
	if home, err := os.UserHomeDir(); err == nil {
		downloads = filepath.Join(home, "Downloads")
	}
	if save {
		path, _ := mygo.Dialog.Save(mygo.SaveDialogOptions{
			Parent: s.window, Title: kind.title, ButtonLabel: kind.button,
			DefaultPath: filepath.Join(downloads, name), Filters: filters, CreateDirectories: true,
		})
		return path
	}
	paths, _ := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Parent: s.window, Title: kind.title, ButtonLabel: kind.button, DefaultPath: downloads, Filters: filters,
	})
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}

// withFile asks for a file and, when one is chosen, hands it to then on the
// main thread. Without a window, as in tests, it runs inline.
func (s *desktop) withFile(save bool, name string, kind fileKind, then func(path string)) {
	if s.window == nil {
		if path := s.chooseFile(save, name, kind); path != "" {
			then(path)
		}
		return
	}
	go func() {
		if path := s.chooseFile(save, name, kind); path != "" {
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
	s.withFile(true, base+packExtension, packFiles(true), func(path string) {
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
	s.withFile(false, "", packFiles(false), func(path string) {
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

// imageFileName suggests a file name for an image: its reference with the
// characters a file name should not hold turned into "-".
func imageFileName(img image) string {
	name := strings.NewReplacer("/", "-", ":", "-", "@", "-").Replace(img.ref())
	return name + ".tar"
}

// saveImage writes an image, with all its tags, into a file docker load
// reads.
func (s *desktop) saveImage(img image) {
	key := "save:" + img.ID
	if s.working[key] {
		return
	}
	s.withFile(true, imageFileName(img), imageFiles(true), func(path string) {
		if filepath.Ext(path) == "" {
			path += ".tar"
		}
		names := img.Tags
		if len(names) == 0 {
			names = []string{img.ID}
		}
		s.working[key] = true
		s.run(packTimeout, func(ctx context.Context) func() {
			err := s.engine.saveImages(ctx, names, path)
			return func() {
				delete(s.working, key)
				s.settle(err, fmt.Sprintf(tr("已打包到 %s", "Packed into %s"), filepath.Base(path)))
			}
		})
	})
}

// loadImage imports an image archive chosen by the user.
func (s *desktop) loadImage() {
	if s.working["load"] {
		return
	}
	s.withFile(false, "", imageFiles(false), func(path string) {
		s.working["load"] = true
		s.run(packTimeout, func(ctx context.Context) func() {
			loaded, err := s.engine.loadImages(ctx, path)
			return func() {
				delete(s.working, "load")
				done := tr("已导入", "Imported")
				switch {
				case len(loaded) == 1:
					done = fmt.Sprintf(tr("已导入 %s", "Imported %s"), loaded[0])
				case len(loaded) > 1:
					done = fmt.Sprintf(tr("已导入 %d 个镜像", "Imported %d images"), len(loaded))
				}
				s.settle(err, done)
			}
		})
	})
}

// saveImages streams the engine's archive of names into path. The file
// appears only when complete.
func (e engineClient) saveImages(ctx context.Context, names []string, path string) error {
	q := url.Values{"names": names}
	return e.download(ctx, "/images/get?"+q.Encode(), path)
}

// download streams the body of a GET of the engine's path into the file
// at dest, which appears only when complete.
func (e engineClient) download(ctx context.Context, path, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+path, nil)
	if err != nil {
		return err
	}
	// Reading the engine's store must not extend the lifetime of an idle VM.
	req.Header.Set("X-Fabo-Background", "1")
	res, err := e.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return responseError(res)
	}
	return writeWhole(dest, func(w io.Writer) error {
		_, err := io.Copy(w, res.Body)
		return err
	})
}

// writeWhole writes the file at path through write, into a hidden partial
// file beside it that takes the name only once complete, so a failure
// leaves no half-written file behind.
func writeWhole(path string, write func(io.Writer) error) (err error) {
	partial, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.partial")
	if err != nil {
		return err
	}
	defer func() {
		partial.Close()
		if err != nil {
			os.Remove(partial.Name())
		}
	}()
	if err = write(partial); err != nil {
		return err
	}
	if err = partial.Close(); err != nil {
		return err
	}
	// CreateTemp makes the file private; what is saved is an ordinary file.
	if err = os.Chmod(partial.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(partial.Name(), path)
}

// loadImages sends an image archive to the engine and returns the images
// it loaded, by name, or by ID when untagged.
func (e engineClient) loadImages(ctx context.Context, path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/images/load?quiet=0", f)
	if err != nil {
		return nil, err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/x-tar")
	res, err := e.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, responseError(res)
	}
	var loaded []string
	dec := json.NewDecoder(res.Body)
	for {
		var step struct {
			Stream, Error string
			ErrorDetail   struct{ Message string }
		}
		if err := dec.Decode(&step); err == io.EOF {
			return loaded, nil
		} else if err != nil {
			return loaded, err
		}
		if msg := step.ErrorDetail.Message; msg != "" || step.Error != "" {
			if msg == "" {
				msg = step.Error
			}
			return loaded, errors.New(msg)
		}
		line := strings.TrimSpace(step.Stream)
		if name, ok := strings.CutPrefix(line, "Loaded image: "); ok {
			loaded = append(loaded, name)
		} else if id, ok := strings.CutPrefix(line, "Loaded image ID: "); ok {
			loaded = append(loaded, shortID(id))
		}
	}
}
