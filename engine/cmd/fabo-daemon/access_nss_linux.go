package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

type accessGroup struct {
	gid     uint32
	members []string
}

// getent uses the system NSS configuration even in the static daemon build.
func getent(parent context.Context, database string, keys ...string) ([]string, error) {
	ctx, cancel := context.WithTimeout(parent, accessRefresh)
	defer cancel()
	args := append([]string{"--", database}, keys...)
	out, err := exec.CommandContext(ctx, "/usr/bin/getent", args...).Output()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err == nil:
	case errors.As(err, &exit) && exit.ExitCode() == 2:
		// For multiple keys, the matching entries still appear in stdout.
	default:
		return nil, fmt.Errorf("read %s accounts: %w", database, err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"), nil
}

func readAccessGroup(ctx context.Context, name string) (*accessGroup, error) {
	rows, err := getent(ctx, "group", name)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	fields := strings.Split(rows[0], ":")
	if len(rows) != 1 || len(fields) != 4 || fields[0] != name {
		return nil, fmt.Errorf("invalid access group record for %q", name)
	}
	gid, err := strconv.ParseUint(fields[2], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid access group ID for %q", name)
	}
	g := &accessGroup{gid: uint32(gid)}
	for member := range strings.SplitSeq(fields[3], ",") {
		if member != "" {
			g.members = append(g.members, member)
		}
	}
	return g, nil
}

type accessAccount struct {
	name     string
	uid, gid uint32
}

func parseAccessAccount(row string) (accessAccount, error) {
	fields := strings.Split(row, ":")
	if len(fields) < 4 || fields[0] == "" {
		return accessAccount{}, errors.New("invalid passwd record")
	}
	uid, uidErr := strconv.ParseUint(fields[2], 10, 32)
	gid, gidErr := strconv.ParseUint(fields[3], 10, 32)
	if uidErr != nil || gidErr != nil {
		return accessAccount{}, errors.New("invalid passwd account IDs")
	}
	return accessAccount{name: fields[0], uid: uint32(uid), gid: uint32(gid)}, nil
}

func (g *accessGroup) allows(ctx context.Context, uid uint32) (bool, error) {
	if g == nil {
		return false, nil
	}
	rows, err := getent(ctx, "passwd", strconv.FormatUint(uint64(uid), 10))
	if err != nil || len(rows) == 0 {
		return false, err
	}
	a, err := parseAccessAccount(rows[0])
	if err != nil {
		return false, err
	}
	return len(rows) == 1 && a.uid == uid && (a.gid == g.gid || slices.Contains(g.members, a.name)), nil
}

func (g *accessGroup) aclUIDs(ctx context.Context) ([]uint32, error) {
	if g == nil || len(g.members) == 0 {
		return nil, nil
	}
	rows, err := getent(ctx, "passwd", g.members...)
	if err != nil {
		return nil, err
	}
	var ids []uint32
	for _, row := range rows {
		a, err := parseAccessAccount(row)
		if err != nil {
			return nil, err
		}
		if a.uid != 0 && slices.Contains(g.members, a.name) {
			ids = append(ids, a.uid)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}
