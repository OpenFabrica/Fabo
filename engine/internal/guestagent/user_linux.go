package guestagent

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type identity struct {
	uid, gid int
	groups   []int
}

// identityFile reads account data after pivot_root. Only ordinary files
// are accepted so an image cannot make user lookup block on a pipe or device.
func identityFile(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", err
	}
	if len(data) > limit {
		return "", fmt.Errorf("%s is too large", path)
	}
	return string(data), nil
}

func accountID(raw string) (int, bool) {
	id, err := strconv.ParseUint(raw, 10, 32)
	// Linux reserves all-bits-one as "do not change" for setresuid/setresgid.
	return int(id), err == nil && id != 1<<32-1
}

func lookupIdentity(raw, passwd, groups string) (identity, error) {
	user, group, explicitGroup := strings.Cut(raw, ":")
	if user == "" && raw != "" {
		return identity{}, fmt.Errorf("empty container user")
	}
	if user == "" {
		user = "0"
	}
	uid, numeric := accountID(user)
	out := identity{uid: uid, gid: uid}
	// The empty user has root's default GID. An explicitly numeric user
	// without a passwd entry uses the same numeric GID, as Fabo does.
	name := ""
	for line := range strings.SplitSeq(passwd, "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 4 {
			continue
		}
		id, idOK := accountID(fields[2])
		gid, gidOK := accountID(fields[3])
		if !idOK || !gidOK || (numeric && id != uid) || (!numeric && fields[0] != user) {
			continue
		}
		out.uid = id
		if raw != "" {
			out.gid = gid
		}
		name = fields[0]
		break
	}
	if !numeric && name == "" {
		return identity{}, fmt.Errorf("unknown container user %q", user)
	}
	if explicitGroup {
		if gid, ok := groupID(group, groups); ok {
			out.gid = gid
			return out, nil
		}
		return identity{}, fmt.Errorf("unknown container group %q", group)
	}
	if name != "" {
		for line := range strings.SplitSeq(groups, "\n") {
			fields := strings.Split(line, ":")
			if len(fields) < 4 {
				continue
			}
			gid, ok := accountID(fields[2])
			if !ok || slices.Contains(out.groups, gid) {
				continue
			}
			for member := range strings.SplitSeq(fields[3], ",") {
				if strings.TrimSpace(member) == name {
					out.groups = append(out.groups, gid)
					break
				}
			}
		}
	}
	return out, nil
}

func groupID(name, groups string) (int, bool) {
	if gid, ok := accountID(name); ok {
		return gid, true
	}
	if name != "" {
		for line := range strings.SplitSeq(groups, "\n") {
			fields := strings.Split(line, ":")
			if len(fields) >= 3 && fields[0] == name {
				if gid, ok := accountID(fields[2]); ok {
					return gid, true
				}
			}
		}
	}
	return 0, false
}

func addSupplementaryGroups(user identity, names []string, groups string) (identity, error) {
	for _, name := range names {
		gid, ok := groupID(name, groups)
		if !ok {
			return identity{}, fmt.Errorf("unknown supplementary container group %q", name)
		}
		if !slices.Contains(user.groups, gid) {
			user.groups = append(user.groups, gid)
		}
	}
	return user, nil
}

func containerIdentity(user string, added []string) (identity, error) {
	passwd, err := identityFile("/etc/passwd")
	if err != nil {
		return identity{}, err
	}
	group, err := identityFile("/etc/group")
	if err != nil {
		return identity{}, err
	}
	resolved, err := lookupIdentity(user, passwd, group)
	if err != nil {
		return identity{}, err
	}
	return addSupplementaryGroups(resolved, added, group)
}
