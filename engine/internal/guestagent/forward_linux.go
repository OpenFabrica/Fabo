package guestagent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

func (refs forwardingRefs) forwardBridge(bridge netlink.Link, subnet netip.Prefix, routes []netlink.Route, internal, vm bool) (func(), error) {
	var uplink *netlink.Route
	for i := range routes {
		r := &routes[i]
		if r.Dst != nil {
			ones, _ := r.Dst.Mask.Size()
			if ones != 0 {
				continue
			}
		}
		if r.LinkIndex > 0 && (uplink == nil || r.Priority < uplink.Priority) {
			uplink = r
		}
	}
	ipv6 := subnet.Addr().Is6()
	var uplinkName string
	if !ipv6 && uplink != nil && !internal {
		link, err := netlink.LinkByIndex(uplink.LinkIndex)
		if err != nil {
			return nil, err
		}
		uplinkName = link.Attrs().Name
	}
	conn, err := nftables.New()
	if err != nil {
		return nil, err
	}
	tableName := "fabo_go"
	if bridge.Attrs().Name != guestwire.DefaultBridge {
		tableName += "_" + bridge.Attrs().Name
	}
	family := nftables.TableFamilyIPv4
	if ipv6 {
		family = nftables.TableFamilyIPv6
	}
	table := conn.CreateTable(&nftables.Table{Name: tableName, Family: family})
	forward := conn.AddChain(&nftables.Chain{Table: table, Name: "forward", Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter})
	add := func(chain *nftables.Chain, result expr.Any, matches ...[]expr.Any) {
		var rule []expr.Any
		for _, match := range matches {
			rule = append(rule, match...)
		}
		conn.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: append(rule, result)})
	}
	onBridge := iface(expr.MetaKeyIIF, bridge.Attrs().Index)
	toBridge := iface(expr.MetaKeyOIF, bridge.Attrs().Index)
	accept := &expr.Verdict{Kind: expr.VerdictAccept}
	drop := &expr.Verdict{Kind: expr.VerdictDrop}
	add(forward, accept, onBridge, toBridge)
	if vm && !internal && family == nftables.TableFamilyIPv4 {
		nat := conn.AddChain(&nftables.Chain{Table: table, Name: "host_bound", Type: nftables.ChainTypeNAT,
			Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityNATDest})
		host := netip.MustParseAddr(guestwire.HostRelayAddress).As4()
		// Queries to the host address use the resolver for the ingress
		// bridge. Pods can reach it without exposing other bridges' names.
		for _, protocol := range []byte{unix.IPPROTO_TCP, unix.IPPROTO_UDP} {
			add(nat, &expr.Redir{RegisterProtoMin: 1}, onBridge, sourceSubnet(subnet), []expr.Any{
				&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{protocol}},
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: host[:]},
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binary.BigEndian.AppendUint16(nil, 53)},
				&expr.Immediate{Register: 1, Data: binary.BigEndian.AppendUint16(nil, 53)},
			})
		}
		add(nat, &expr.Redir{RegisterProtoMin: 1}, onBridge, sourceSubnet(subnet), []expr.Any{
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: host[:]},
			&expr.Immediate{Register: 1, Data: binary.BigEndian.AppendUint16(nil, guestwire.HostRelayPort)},
		})
	}
	forwarding := !internal && (ipv6 || uplinkName != "")
	if forwarding {
		nat := conn.AddChain(&nftables.Chain{Table: table, Name: "postrouting", Type: nftables.ChainTypeNAT,
			Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource})
		var onUplink, toUplink []expr.Any
		if ipv6 {
			// Like Rust Fabo, let the route choose the external interface.
			// This includes directly connected subnets on a non-default uplink.
			onUplink = outsideFabo(expr.MetaKeyIIFNAME)
			toUplink = outsideFabo(expr.MetaKeyOIFNAME)
		} else {
			onUplink = iface(expr.MetaKeyIIF, uplink.LinkIndex)
			toUplink = iface(expr.MetaKeyOIF, uplink.LinkIndex)
		}
		source := sourceSubnet(subnet)
		// Postrouting exposes the output interface, not the ingress.
		add(nat, &expr.Masq{}, toUplink, source)
		add(forward, accept, onBridge, toUplink, source)
		add(forward, accept, onUplink, toBridge, []expr.Any{
			&expr.Ct{Key: expr.CtKeySTATE, Register: 1},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
				Mask: binary.NativeEndian.AppendUint32(nil, expr.CtStateBitESTABLISHED|expr.CtStateBitRELATED), Xor: make([]byte, 4)},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: make([]byte, 4)},
		})
	}
	add(forward, drop, onBridge)
	add(forward, drop, toBridge)
	if err := conn.Flush(); err != nil {
		return nil, fmt.Errorf("install bridge firewall: %w", err)
	}
	deleteTable := func() error {
		conn.DelTable(table)
		return conn.Flush()
	}
	var restore []func() error
	if forwarding {
		var settings []forwardingSetting
		if family == nftables.TableFamilyIPv6 {
			settings = append(settings, forwardingSetting{ipv6ForwardingPath, "1"})
		} else {
			for _, name := range []string{bridge.Attrs().Name, uplinkName} {
				settings = append(settings, forwardingSetting{"/proc/sys/net/ipv4/conf/" + name + "/forwarding", "1"})
			}
		}
		for _, setting := range settings {
			reset, err := refs.acquire(setting)
			if err != nil {
				for i := len(restore) - 1; i >= 0; i-- {
					err = errors.Join(err, restore[i]())
				}
				return nil, errors.Join(err, deleteTable())
			}
			restore = append(restore, reset)
		}
	}
	return func() {
		for i := len(restore) - 1; i >= 0; i-- {
			if err := restore[i](); err != nil && !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(os.Stderr, "restore bridge forwarding:", err)
			}
		}
		if err := deleteTable(); err != nil {
			fmt.Fprintln(os.Stderr, "remove bridge firewall:", err)
		}
	}, nil
}

type forwardingRef struct {
	count int
	reset func() error
}

// All references are changed while the runtime owns bridge operations.
type forwardingRefs map[string]*forwardingRef

type forwardingSetting struct{ path, value string }

const ipv6ForwardingPath = "/proc/sys/net/ipv6/conf/all/forwarding"

func (refs forwardingRefs) acquire(setting forwardingSetting) (func() error, error) {
	ref := refs[setting.path]
	if ref == nil {
		apply := setForwarding
		if setting.path == ipv6ForwardingPath {
			apply = enableIPv6Forwarding
		}
		reset, err := apply(setting)
		if err != nil {
			return nil, err
		}
		ref = &forwardingRef{reset: reset}
		refs[setting.path] = ref
	}
	ref.count++
	return func() error {
		ref.count--
		if ref.count == 0 {
			delete(refs, setting.path)
			return ref.reset()
		}
		return nil
	}, nil
}

func setForwarding(setting forwardingSetting) (func() error, error) {
	path := setting.path
	before, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(before)) == setting.value {
		return func() error { return nil }, nil
	}
	if err := os.WriteFile(path, []byte(setting.value), 0); err != nil {
		return nil, err
	}
	return func() error { return os.WriteFile(path, before, 0) }, nil
}

// Global forwarding would otherwise stop router advertisements on every
// interface that accepts them only in host mode. Keep those interfaces in
// router-aware mode for the lifetime of Fabo's forwarding reference.
func enableIPv6Forwarding(setting forwardingSetting) (func() error, error) {
	before, err := os.ReadFile(setting.path)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(before)) == setting.value {
		return func() error { return nil }, nil
	}
	const conf = "/proc/sys/net/ipv6/conf/"
	entries, err := os.ReadDir(conf)
	if err != nil {
		return nil, err
	}
	var undo []func() error
	restore := func() error {
		var err error
		for i := len(undo) - 1; i >= 0; i-- {
			if resetErr := undo[i](); !errors.Is(resetErr, os.ErrNotExist) {
				err = errors.Join(err, resetErr)
			}
		}
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "all" || name == "default" || name == "lo" || strings.HasPrefix(name, "fabo") || strings.HasPrefix(name, "kv") {
			continue
		}
		path := conf + name + "/accept_ra"
		mode, err := os.ReadFile(path)
		if err != nil || strings.TrimSpace(string(mode)) != "1" {
			continue
		}
		reset, err := setForwarding(forwardingSetting{path, "2"})
		if err != nil {
			return nil, errors.Join(err, restore())
		}
		undo = append(undo, reset)
	}
	reset, err := setForwarding(setting)
	if err != nil {
		return nil, errors.Join(err, restore())
	}
	undo = append(undo, reset)
	return restore, nil
}

func iface(key expr.MetaKey, index int) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: key, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binary.NativeEndian.AppendUint32(nil, uint32(index))},
	}
}

func outsideFabo(key expr.MetaKey) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: key, Register: 1},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte("fabo")},
	}
}

func sourceSubnet(subnet netip.Prefix) []expr.Any {
	bits := subnet.Addr().BitLen()
	size, offset := uint32(bits/8), uint32(12)
	if bits == 128 {
		offset = 8
	}
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: offset, Len: size},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: size, Mask: net.CIDRMask(subnet.Bits(), bits), Xor: make([]byte, size)},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: subnet.Addr().AsSlice()},
	}
}
