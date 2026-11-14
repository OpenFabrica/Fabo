package main

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
)

// listScript lists the directory given as $1, one entry per line: "L|name"
// for each symbolic link, then "S|type|size|mtime|name" from stat, which
// follows links. A container without stat gets "N|name" from ls -1Ap, a
// directory's name ending in a slash. Globs that match nothing stay as
// they are and stat skips them.
const listScript = `cd -- "$1" || exit 1
set -- .[!.]* ..?* *
for f; do [ -L "$f" ] && printf 'L|%s\n' "$f"; done
if command -v stat >/dev/null 2>&1; then stat -L -c 'S|%F|%s|%Y|%n' -- "$@" 2>/dev/null; else ls -1Ap | sed 's/^/N|/'; fi
exit 0`

// previewScript prints the regular file $1, or a NUL-led marker with its
// size when it is larger than the 256 KB the preview shows, as Kiri's.
const previewScript = `[ -f "$1" ] || { echo "not a regular file"; exit 2; }
n=$(wc -c < "$1")
if [ "$n" -gt 262144 ]; then printf '\000FABO-TOO-LARGE %s' "$n"; exit 0; fi
cat -- "$1"`

// fileEntry is one line of a listed directory.
type fileEntry struct {
	name       string
	dir, link  bool
	size       int64
	mtime      int64
	statsKnown bool
}

// filePreview is the file the Files tab shows instead of the list.
type filePreview struct {
	path, text, problem string
	loaded, binary      bool
	size                int64
}

// parseListing reads listScript's output: directories first, then files,
// each sorted by name.
func parseListing(out string) []fileEntry {
	links := map[string]bool{}
	var entries []fileEntry
	for _, line := range strings.Split(out, "\n") {
		kind, rest, ok := strings.Cut(line, "|")
		if !ok {
			continue
		}
		switch kind {
		case "L":
			links[rest] = true
		case "N":
			if rest = strings.TrimSpace(rest); rest != "" {
				entries = append(entries, fileEntry{name: strings.TrimSuffix(rest, "/"), dir: strings.HasSuffix(rest, "/")})
			}
		case "S":
			parts := strings.SplitN(rest, "|", 4)
			if len(parts) != 4 {
				continue
			}
			size, _ := strconv.ParseInt(parts[1], 10, 64)
			mtime, _ := strconv.ParseInt(parts[2], 10, 64)
			entries = append(entries, fileEntry{name: parts[3], dir: parts[0] == "directory", size: size, mtime: mtime, statsKnown: true})
		}
	}
	for i := range entries {
		entries[i].link = links[entries[i].name]
	}
	// A link stat cannot follow (it points nowhere) still shows.
	for name := range links {
		if !slices.ContainsFunc(entries, func(e fileEntry) bool { return e.name == name }) {
			entries = append(entries, fileEntry{name: name, link: true})
		}
	}
	slices.SortFunc(entries, func(a, b fileEntry) int {
		if a.dir != b.dir {
			if a.dir {
				return -1
			}
			return 1
		}
		return strings.Compare(a.name, b.name)
	})
	return entries
}

// joinPath puts a name under a directory path that ends in a slash.
func joinPath(dir, name string) string { return dir + name }

// filesTab browses the container's filesystem one directory at a time:
// a path whose parts open their folder, then the entries with their size
// and age. A file opens a preview of its text, with a way to save it to
// Downloads. It needs the container running, as it asks the container.
func (s *desktop) filesTab(c *ui.Context, p palette, x container) {
	if x.State != "running" {
		if empty(c, p, "folder", tr("容器未运行", "The container is not running"), tr("启动", "Start")) {
			s.startContainer(x)
		}
		return
	}
	path := s.filePath[x.ID]
	if path == "" {
		path = "/"
	}
	if want := x.ID + ":" + path; s.detail.filesFor != want {
		s.detail.filesFor, s.detail.files, s.detail.filesErr = want, nil, ""
		key := s.detail.key
		s.run(15*time.Second, func(ctx context.Context) func() {
			out, err := s.engine.exec(ctx, x.ID, []string{"sh", "-c", listScript, "sh", path})
			return func() {
				if s.detail.key != key || s.detail.filesFor != want {
					return
				}
				if err != nil {
					s.detail.filesErr = err.Error()
					return
				}
				s.detail.files = parseListing(out)
			}
		})
	}
	open := func(dir string) {
		s.filePath[x.ID] = dir
		s.detail.preview = filePreview{}
		s.list("files").ScrollTo(0, ui.Start)
	}
	ui.Row(c.Key("crumbs")).FillWidth().Height(40).Shrink(0).Padding(0, 12, 0, 12).Gap(2).AlignItems(ui.Center).
		BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
		if iconButton(c.Key("up"), p, "up", tr("上一级", "Up"), path == "/" && s.detail.preview.path == "").Clicked() {
			if s.detail.preview.path != "" {
				s.detail.preview = filePreview{}
			} else {
				parent := strings.TrimSuffix(path, "/")
				open(parent[:strings.LastIndex(parent, "/")+1])
			}
		}
		// Each part of the path opens its folder; the root is the
		// container's name.
		ui.Row(c.Key("parts")).Grow(1).Width(0).MinWidth(0).Gap(0).AlignItems(ui.Center).Clip().Children(func() {
			crumb := func(key, text, dir string) {
				b := clickable(c.Key(key)).Label(tr("打开 ", "Open ") + dir).Height(26).PaddingX(4).Radius(6).Center().Shrink(0)
				if b.Hovered() {
					b.Background(p.soft)
				}
				b.Children(func() { ui.Text(c, text).Font(mono).FontSize(12).TextColor(p.text).SingleLine() })
				if b.Clicked() {
					open(dir)
				}
			}
			crumb("root", x.Name, "/")
			dir := "/"
			for i, part := range strings.Split(strings.Trim(path, "/"), "/") {
				if part == "" {
					continue
				}
				dir += part + "/"
				ui.Text(c, "/").Font(mono).FontSize(12).TextColor(p.muted).Shrink(0)
				crumb(fmt.Sprint("part-", i), part, dir)
			}
		})
	})
	if s.detail.preview.path != "" {
		s.previewPane(c, p, x)
		return
	}
	if s.detail.filesErr != "" {
		note(c, p, s.detail.filesErr)
		return
	}
	entries := s.detail.files
	if entries == nil {
		note(c, p, tr("正在读取…", "Loading…"))
		return
	}
	if len(entries) == 0 {
		note(c, p, tr("这个文件夹是空的。", "This folder is empty."))
		return
	}
	ui.List(c.Key("files"), s.list("files"), len(entries)).FillWidth().Grow(1).MinHeight(0).Rows(func(r ui.ListRow) {
		e, rc := entries[r.Index], r.Context
		full := joinPath(path, e.name)
		b := clickable(rc).Label(tr("打开 ", "Open ")+e.name).FillWidth().Height(36).PaddingX(16).Gap(10).
			AlignItems(ui.Center).Justify(ui.Start).BorderWidth(0, 0, 1, 0).BorderColor(p.line)
		if b.Hovered() {
			b.Background(p.hover)
		}
		b.Children(func() {
			icon, ink := "file", p.muted
			if e.dir {
				icon, ink = "folder", p.accent
			}
			ui.Icon(rc, icons[icon]).Size(15, 15).Shrink(0).TextColor(ink)
			ui.Row(rc).Grow(1).Width(0).MinWidth(0).Gap(6).AlignItems(ui.Center).Children(func() {
				label(rc, e.name, p.text).FontSize(12.5)
				if e.link {
					ui.Text(rc, "↪").FontSize(12).TextColor(p.muted).Shrink(0)
				}
			})
			if e.statsKnown && !e.dir {
				ui.Text(rc, displaySize(e.size)).FontSize(12).FontFeatures("tnum").TextColor(p.muted).Shrink(0)
			}
			if e.mtime > 0 {
				ui.Text(rc, since(s.now, e.mtime)).FontSize(12).TextColor(p.muted).Width(72).Shrink(0).TextAlign(ui.End)
			}
		})
		if b.Clicked() {
			if e.dir {
				open(full + "/")
			} else {
				s.openPreview(x, full, e.size)
			}
		}
	})
}

// openPreview reads a file for the Files tab off the main thread.
func (s *desktop) openPreview(x container, path string, size int64) {
	s.detail.preview = filePreview{path: path, size: size}
	key := s.detail.key
	s.run(15*time.Second, func(ctx context.Context) func() {
		out, err := s.engine.exec(ctx, x.ID, []string{"sh", "-c", previewScript, "sh", path})
		return func() {
			pv := &s.detail.preview
			if s.detail.key != key || pv.path != path {
				return
			}
			pv.loaded = true
			switch {
			case err != nil:
				pv.problem = err.Error()
			case strings.HasPrefix(out, "\x00FABO-TOO-LARGE"):
				pv.binary = true
				pv.size, _ = strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(out, "\x00FABO-TOO-LARGE")), 10, 64)
			case strings.ContainsRune(out, 0) || !utf8.ValidString(out):
				pv.binary = true
			default:
				pv.text = out
			}
		}
	})
}

// previewPane shows the opened file: its name, a button to save it to
// Downloads, and its text in the dark well, or why it cannot be shown.
func (s *desktop) previewPane(c *ui.Context, p palette, x container) {
	pv := s.detail.preview
	ui.Row(c.Key("file-bar")).FillWidth().Shrink(0).Padding(10, 16).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Icon(c, icons["file"]).Size(15, 15).Shrink(0).TextColor(p.muted)
		ui.Text(c, filepath.Base(pv.path)).FontSize(13).FontWeight(500).TextColor(p.text).SingleLine().Grow(1).MinWidth(0)
		if pv.size > 0 {
			ui.Text(c, displaySize(pv.size)).FontSize(12).FontFeatures("tnum").TextColor(p.muted).Shrink(0)
		}
		if button(c.Key("save-file"), p, tr("保存到下载", "Save to Downloads"), "download", plainButton, s.working["download"]) {
			s.downloadFile(x, pv.path)
		}
	})
	switch {
	case !pv.loaded:
		note(c, p, tr("正在读取…", "Loading…"))
	case pv.problem != "":
		note(c, p, pv.problem)
	case pv.binary:
		note(c, p, tr("这个文件是二进制文件或大于 256 KB，请保存到下载后打开。", "This file is binary or larger than 256 KB; save it to Downloads to open it."))
	case pv.text == "":
		note(c, p, tr("这个文件是空的。", "This file is empty."))
	default:
		lines := strings.Split(strings.TrimSuffix(pv.text, "\n"), "\n")
		s.logWell(c, "file-preview", lines, numbered(lines, 1), false, false)
	}
}

// downloadFile copies a file out of the container into ~/Downloads through
// the engine's archive endpoint, keeping a file already there under its
// name ("name 2.ext"), then shows it in the Finder on macOS.
func (s *desktop) downloadFile(x container, path string) {
	s.working["download"] = true
	engine := s.engine
	s.run(5*time.Minute, func(ctx context.Context) func() {
		to, err := engine.saveFromContainer(ctx, x.ID, path)
		return func() {
			delete(s.working, "download")
			if err != nil {
				s.notices = append(s.notices, fmt.Sprintf(tr("无法保存文件：%v", "Could not save the file: %v"), err))
				return
			}
			s.notices = append(s.notices, fmt.Sprintf(tr("已保存到下载：%s", "Saved to Downloads as %s"), filepath.Base(to)))
			if runtime.GOOS == "darwin" && s.window != nil {
				_ = exec.Command("open", "-R", to).Start()
			}
		}
	})
	if s.engine.http == nil {
		delete(s.working, "download")
	}
}

// saveFromContainer reads one file as the tar the archive endpoint sends
// and writes it into ~/Downloads under a name not taken yet. The archive
// holds a link as a link, so a linked path is read again at the link's
// target, a few times at most, keeping the name that was asked for.
func (e engineClient) saveFromContainer(ctx context.Context, id, path string) (string, error) {
	ctx = context.WithValue(ctx, backgroundRead{}, true)
	name := filepath.Base(path)
	for range 5 {
		res, archive, head, err := e.archiveEntry(ctx, id, path)
		if err != nil {
			return "", err
		}
		switch head.Typeflag {
		case tar.TypeSymlink:
			res.Body.Close()
			target := head.Linkname
			if !strings.HasPrefix(target, "/") {
				target = filepath.ToSlash(filepath.Join(filepath.Dir(path), target))
			}
			path = target
			continue
		case tar.TypeReg:
			defer res.Body.Close()
			return writeDownload(name, archive)
		}
		res.Body.Close()
		return "", errors.New("not a regular file")
	}
	return "", errors.New("too many links")
}

// archiveEntry asks the archive endpoint for one path and reads the header
// of its first entry; the caller closes the response.
func (e engineClient) archiveEntry(ctx context.Context, id, path string) (*http.Response, *tar.Reader, *tar.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+containerPath(id)+"/archive?path="+url.QueryEscape(path), nil)
	if err != nil {
		return nil, nil, nil, err
	}
	req.Header.Set("X-Kiri-Background", "1")
	res, err := e.http.Do(req)
	if err != nil {
		return nil, nil, nil, err
	}
	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		return nil, nil, nil, responseError(res)
	}
	archive := tar.NewReader(res.Body)
	head, err := archive.Next()
	if err != nil {
		res.Body.Close()
		return nil, nil, nil, fmt.Errorf("the archive is empty: %w", err)
	}
	return res, archive, head, nil
}

// writeDownload copies content into ~/Downloads as name, or as "name 2.ext"
// and so on when that is taken, and returns where it went.
func writeDownload(name string, content io.Reader) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		return "", err
	}
	ext := filepath.Ext(name)
	to := filepath.Join(downloads, name)
	for n := 2; ; n++ {
		if _, err := os.Lstat(to); errors.Is(err, os.ErrNotExist) {
			break
		}
		to = filepath.Join(downloads, fmt.Sprintf("%s %d%s", strings.TrimSuffix(name, ext), n, ext))
	}
	file, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(file, content); err != nil {
		file.Close()
		os.Remove(to)
		return "", err
	}
	return to, file.Close()
}
