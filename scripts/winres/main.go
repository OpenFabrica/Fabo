// winres writes the Windows resource object (.syso) that gives Fabo's
// .exe files their icon and version: the icon for Explorer, Start and a
// pinned taskbar button, the version for Explorer's Details and for
// Windows Installer, which replaces a file by comparing versions. A file
// with none it judges by its dates, and may keep an old one in an upgrade.
// Go links a .syso in the package directory into the build for its
// GOOS/GOARCH (rsrc_windows_amd64.syso), so scripts/windows-bundle.sh writes
// one before each build and removes it after.
//
//	go -C scripts/winres run . -icon ICO -version X.Y.Z -name NAME -desc TEXT -arch amd64|arm64 -o OUT.syso
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tc-hib/winres"
	"github.com/tc-hib/winres/version"
)

func main() {
	icon := flag.String("icon", "", "the .ico file")
	ver := flag.String("version", "0.0.0", "the version, x.y.z[-suffix]")
	name := flag.String("name", "", "the program's file name without .exe")
	desc := flag.String("desc", "Fabo", "the file description")
	arch := flag.String("arch", "amd64", "amd64 or arm64")
	out := flag.String("o", "", "the .syso to write")
	flag.Parse()
	if err := run(*icon, *ver, *name, *desc, *arch, *out); err != nil {
		fmt.Fprintln(os.Stderr, "winres:", err)
		os.Exit(1)
	}
}

// numbers is the version as Windows' four numbers: 0.5.2-dev.3 is 0.5.2.0.
func numbers(v string) [4]uint16 {
	v, _, _ = strings.Cut(strings.TrimPrefix(v, "v"), "-")
	var n [4]uint16
	for i, part := range strings.SplitN(v, ".", 4) {
		x, _ := strconv.ParseUint(part, 10, 16)
		n[i] = uint16(x)
	}
	return n
}

// run builds the resource set and writes it as a COFF object.
func run(iconPath, ver, name, desc, arch, out string) error {
	if iconPath == "" || name == "" || out == "" {
		return fmt.Errorf("-icon, -name and -o are required")
	}
	rs := &winres.ResourceSet{}
	f, err := os.Open(iconPath)
	if err != nil {
		return err
	}
	ico, err := winres.LoadICO(f)
	f.Close()
	if err != nil {
		return err
	}
	if err := rs.SetIcon(winres.ID(1), ico); err != nil {
		return err
	}
	n := numbers(ver)
	vi := version.Info{}
	vi.SetFileVersion(fmt.Sprintf("%d.%d.%d.%d", n[0], n[1], n[2], n[3]))
	vi.SetProductVersion(fmt.Sprintf("%d.%d.%d.%d", n[0], n[1], n[2], n[3]))
	for k, v := range map[string]string{
		"CompanyName": "OpenFabrica", "FileDescription": desc, "FileVersion": ver,
		"InternalName": name, "OriginalFilename": name + ".exe", "ProductName": "Fabo", "ProductVersion": ver,
		"LegalCopyright": "Fabo contributors",
	} {
		if err := vi.Set(version.LangDefault, k, v); err != nil {
			return err
		}
	}
	rs.SetVersionInfo(vi)
	target := winres.ArchAMD64
	if arch == "arm64" {
		target = winres.ArchARM64
	}
	w, err := os.Create(out)
	if err != nil {
		return err
	}
	if err := rs.WriteObject(w, target); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}
