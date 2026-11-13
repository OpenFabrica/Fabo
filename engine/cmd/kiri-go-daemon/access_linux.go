package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const accessRefresh = 2 * time.Second

func socketAccessGroup() string { return os.Getenv("KIRI_SOCKET_GROUP") }

func configureSocket(parent context.Context, listener *net.UnixListener, name string) (net.Listener, error) {
	if name == "" {
		return listener, nil
	}
	group, err := readAccessGroup(parent, name)
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, fmt.Errorf("socket access group %q does not exist", name)
	}
	path := listener.Addr().String()
	if err := os.Chown(path, -1, int(group.gid)); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0660); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	l := &accessListener{
		UnixListener: listener, ctx: ctx, cancel: cancel, group: name,
		owner: uint32(os.Geteuid()), open: make(map[*accessConn]uint32),
	}
	l.updateACL(group)
	go l.watch()
	return l, nil
}

// accessListener checks the peer UID against the current account database.
// The kernel ACL lets newly added members reach the gate without relogin.
type accessListener struct {
	*net.UnixListener
	ctx    context.Context
	cancel context.CancelFunc
	group  string
	owner  uint32

	mu   sync.Mutex
	open map[*accessConn]uint32
	// Only the startup code and then the watcher update the ACL state.
	listed         []uint32
	aclSet         bool
	aclUnavailable bool
}

func (l *accessListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			return nil, err
		}
		uid, err := socketPeerUID(conn)
		if err != nil {
			_ = conn.Close()
			continue
		}
		if uid == 0 || uid == l.owner {
			return conn, nil
		}
		group, err := readAccessGroup(l.ctx, l.group)
		var allowed bool
		if err == nil {
			allowed, err = group.allows(l.ctx, uid)
		}
		if err != nil || !allowed {
			_ = conn.Close()
			if err != nil && l.ctx.Err() == nil {
				log.Printf("socket access check: %v", err)
			}
			continue
		}
		c := &accessConn{UnixConn: conn, listener: l}
		l.mu.Lock()
		if err := l.ctx.Err(); err != nil {
			l.mu.Unlock()
			_ = conn.Close()
			return nil, net.ErrClosed
		}
		l.open[c] = uid
		l.mu.Unlock()
		return c, nil
	}
}

func (l *accessListener) Close() error {
	l.cancel()
	err := l.UnixListener.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	for c := range l.open {
		_ = c.UnixConn.Close()
	}
	clear(l.open)
	return err
}

type accessConn struct {
	*net.UnixConn
	listener *accessListener
}

func (c *accessConn) Close() error {
	c.listener.mu.Lock()
	delete(c.listener.open, c)
	c.listener.mu.Unlock()
	return c.UnixConn.Close()
}

func socketPeerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var lookupErr error
	if err := raw.Control(func(fd uintptr) {
		cred, lookupErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if lookupErr != nil {
		return 0, lookupErr
	}
	return cred.Uid, nil
}

func (l *accessListener) watch() {
	tick := time.NewTicker(accessRefresh)
	defer tick.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-tick.C:
			l.refresh()
		}
	}
}

func (l *accessListener) refresh() {
	group, err := readAccessGroup(l.ctx, l.group)
	if err != nil {
		if l.ctx.Err() == nil {
			log.Printf("recheck socket access: %v", err)
		}
		return
	}
	l.mu.Lock()
	uids := make(map[uint32]bool)
	for _, uid := range l.open {
		uids[uid] = false
	}
	l.mu.Unlock()
	for uid := range uids {
		allowed, err := group.allows(l.ctx, uid)
		// An unavailable account database leaves existing sessions open
		// until the next recheck. Admission still requires a valid answer.
		uids[uid] = err == nil && !allowed
	}
	l.mu.Lock()
	for conn, uid := range l.open {
		if uids[uid] {
			delete(l.open, conn)
			_ = conn.UnixConn.Close()
		}
	}
	l.mu.Unlock()
	l.updateACL(group)
}

func (l *accessListener) updateACL(group *accessGroup) {
	if l.aclUnavailable {
		return
	}
	ids, err := group.aclUIDs(l.ctx)
	if err == nil && (!l.aclSet || !slices.Equal(l.listed, ids)) {
		err = unix.Lsetxattr(l.Addr().String(), "system.posix_acl_access", socketACL(ids), 0)
		if err == nil {
			l.listed, l.aclSet = ids, true
		}
	}
	if errors.Is(err, unix.EOPNOTSUPP) {
		l.aclUnavailable = true
	}
	if err != nil && l.ctx.Err() == nil {
		log.Printf("socket access list: %v", err)
	}
}

func socketACL(ids []uint32) []byte {
	// Linux POSIX ACL xattr version 2, in tag and UID order.
	data := binary.LittleEndian.AppendUint32(nil, 2)
	entry := func(tag, mode uint16, id uint32) {
		data = binary.LittleEndian.AppendUint16(data, tag)
		data = binary.LittleEndian.AppendUint16(data, mode)
		data = binary.LittleEndian.AppendUint32(data, id)
	}
	const noID = ^uint32(0)
	entry(1, 6, noID) // owner
	for _, uid := range ids {
		entry(2, 6, uid)
	}
	entry(4, 6, noID)    // owning group
	entry(0x10, 6, noID) // mask
	entry(0x20, 0, noID) // others
	return data
}
