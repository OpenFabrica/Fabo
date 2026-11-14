package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/vishvananda/netlink"
)

const uplinkName = "eth0"
const resolverPath = "/etc/resolv.conf"

type uplinkConfig struct {
	address  netip.Prefix
	router   netip.Addr
	dns      []netip.Addr
	expires  time.Time
	renewAt  time.Time
	lifetime time.Duration
}

// startUplink waits for the first lease before the guest announces readiness.
// This lets the bridge select the correct outgoing interface on its first run.
func startUplink() (func(), error) {
	link, err := netlink.LinkByName(uplinkName)
	if err != nil {
		var missing netlink.LinkNotFoundError
		if errors.As(err, &missing) {
			return func() {}, nil
		}
		return nil, err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, err
	}
	client, err := nclient4.New(uplinkName, nclient4.WithTimeout(500*time.Millisecond))
	if err != nil {
		return nil, fmt.Errorf("open VM DHCP client: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	lease, config, err := acquireUplink(ctx, link, client, nil, uplinkConfig{})
	if err != nil {
		cancel()
		_ = client.Close()
		return nil, fmt.Errorf("configure VM uplink: %w", err)
	}
	log.Printf("VM uplink %s via %s", config.address, config.router)
	done := make(chan struct{})
	go func() {
		defer close(done)
		maintainUplink(ctx, link, client, lease, config)
	}()
	return func() {
		cancel()
		<-done
		_ = client.Close()
	}, nil
}

func acquireUplink(ctx context.Context, link netlink.Link, client *nclient4.Client, lease *nclient4.Lease, previous uplinkConfig) (*nclient4.Lease, uplinkConfig, error) {
	timeout := 5 * time.Second
	if lease != nil {
		timeout = min(timeout, time.Until(previous.expires))
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	options := dhcpv4.WithRequestedOptions(dhcpv4.OptionSubnetMask, dhcpv4.OptionRouter, dhcpv4.OptionDomainNameServer)
	var next *nclient4.Lease
	var err error
	if lease == nil {
		next, err = client.Request(ctx, options)
	} else {
		next, err = client.Renew(ctx, lease, options)
	}
	if err != nil {
		return nil, uplinkConfig{}, err
	}
	config, err := readUplinkConfig(next, previous)
	if err != nil {
		return nil, uplinkConfig{}, err
	}
	if err := applyUplink(link, config, previous); err != nil {
		return nil, uplinkConfig{}, err
	}
	// The next renewal builds ciaddr from this ACK. Some servers leave
	// yiaddr empty when renewing an address the VM already has.
	next.ACK.YourIPAddr = config.address.Addr().AsSlice()
	return next, config, nil
}

func readUplinkConfig(lease *nclient4.Lease, previous uplinkConfig) (uplinkConfig, error) {
	ack, offer := lease.ACK, lease.Offer
	ip := ipv4(ack.YourIPAddr)
	if !ip.IsGlobalUnicast() {
		ip = previous.address.Addr()
	}
	mask := ack.SubnetMask()
	bits, _ := mask.Size()
	if mask == nil {
		if previous.address.IsValid() {
			bits = previous.address.Bits()
		} else {
			bits, _ = offer.SubnetMask().Size()
		}
	}
	prefix := netip.PrefixFrom(ip, bits)
	router := previous.router
	routers := ack.Router()
	if len(routers) == 0 && !router.IsValid() {
		routers = offer.Router()
	}
	if len(routers) > 0 {
		router = ipv4(routers[0])
	}
	if !ip.IsGlobalUnicast() || bits == 0 || !router.IsGlobalUnicast() || router == ip || !prefix.Contains(router) {
		return uplinkConfig{}, errors.New("DHCP lease lacks a usable IPv4 address, mask or gateway")
	}

	nameservers := ack.DNS()
	if len(nameservers) == 0 && !previous.address.IsValid() {
		nameservers = offer.DNS()
	}
	dns := previous.dns
	if len(nameservers) > 0 {
		dns = nil
		for _, server := range nameservers {
			if addr := ipv4(server); addr.IsGlobalUnicast() {
				dns = append(dns, addr)
			}
		}
	}
	lifetime := ack.IPAddressLeaseTime(previous.lifetime)
	if lifetime <= 0 {
		lifetime = offer.IPAddressLeaseTime(0)
	}
	if lifetime <= 0 {
		return uplinkConfig{}, errors.New("DHCP lease has no valid lifetime")
	}
	renew := ack.IPAddressRenewalTime(lifetime / 2)
	if renew <= 0 || renew >= lifetime {
		renew = lifetime / 2
	}
	return uplinkConfig{
		address: prefix, router: router, dns: dns, lifetime: lifetime,
		expires: lease.CreationTime.Add(lifetime), renewAt: lease.CreationTime.Add(renew),
	}, nil
}

func ipv4(ip net.IP) netip.Addr {
	addr, _ := netip.AddrFromSlice(ip.To4())
	return addr
}

func (c uplinkConfig) addr() *netlink.Addr {
	network := &net.IPNet{IP: c.address.Addr().AsSlice(), Mask: net.CIDRMask(c.address.Bits(), 32)}
	return &netlink.Addr{IPNet: network}
}

func (c uplinkConfig) route(link netlink.Link) *netlink.Route {
	return &netlink.Route{LinkIndex: link.Attrs().Index, Gw: c.router.AsSlice()}
}

func applyUplink(link netlink.Link, config, previous uplinkConfig) error {
	if err := netlink.AddrReplace(link, config.addr()); err != nil {
		return err
	}
	if err := netlink.RouteReplace(config.route(link)); err != nil {
		if config.address != previous.address {
			_ = netlink.AddrDel(link, config.addr())
		}
		return err
	}
	if previous.address.IsValid() && config.address != previous.address {
		_ = netlink.AddrDel(link, previous.addr())
	}

	var resolver strings.Builder
	for _, server := range config.dns {
		fmt.Fprintf(&resolver, "nameserver %s\n", server)
	}
	tmp := resolverPath + ".dhcp"
	if err := os.WriteFile(tmp, []byte(resolver.String()), 0644); err != nil {
		log.Printf("write VM resolver: %v", err)
	} else if err := os.Rename(tmp, resolverPath); err != nil {
		log.Printf("install VM resolver: %v", err)
	}
	return nil
}

func maintainUplink(ctx context.Context, link netlink.Link, client *nclient4.Client, lease *nclient4.Lease, config uplinkConfig) {
	delay := time.Until(config.renewAt)
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if lease != nil && !time.Now().Before(config.expires) {
			clearUplink(link, config)
			lease, config = nil, uplinkConfig{}
		}
		next, nextConfig, err := acquireUplink(ctx, link, client, lease, config)
		if err == nil {
			lease, config = next, nextConfig
			delay = time.Until(config.renewAt)
			continue
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("refresh VM DHCP lease: %v", err)
		var nak *nclient4.ErrNak
		if lease != nil && errors.As(err, &nak) {
			clearUplink(link, config)
			lease, config = nil, uplinkConfig{}
		}
		delay = 5 * time.Second
		if lease != nil {
			delay = min(delay, time.Until(config.expires))
		}
	}
}

func clearUplink(link netlink.Link, config uplinkConfig) {
	_ = netlink.RouteDel(config.route(link))
	_ = netlink.AddrDel(link, config.addr())
	_ = os.Remove(resolverPath)
}
