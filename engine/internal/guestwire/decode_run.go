package guestwire

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

type runDecoder struct {
	data []byte
	err  error
}

func (d *runDecoder) take(n int) []byte {
	if d.err != nil || n > len(d.data) {
		d.err = io.ErrUnexpectedEOF
		return nil
	}
	b := d.data[:n]
	d.data = d.data[n:]
	return b
}

func (d *runDecoder) number(n int) uint64 {
	b := d.take(n)
	if b == nil {
		return 0
	}
	switch n {
	case 1:
		return uint64(b[0])
	case 4:
		return uint64(binary.LittleEndian.Uint32(b))
	default:
		return binary.LittleEndian.Uint64(b)
	}
}

func (d *runDecoder) text() string {
	if d.err != nil {
		return ""
	}
	raw, rest, err := takeBytes(d.data)
	if err != nil {
		d.err = err
		return ""
	}
	d.data = rest
	if !utf8.Valid(raw) {
		d.err = fmt.Errorf("invalid UTF-8 in Run")
		return ""
	}
	return string(raw)
}

func (d *runDecoder) list() []string {
	n := d.number(4)
	if n > uint64(len(d.data)/4) {
		d.err = io.ErrUnexpectedEOF
		return nil
	}
	out := make([]string, 0, n)
	for range n {
		out = append(out, d.text())
		if d.err != nil {
			return nil
		}
	}
	return out
}

func DecodeRun(f Frame) (RunSpec, error) {
	d := runDecoder{data: f.Payload}
	var s RunSpec
	s.ID = d.text()
	s.Layers, s.Argv, s.Env = d.list(), d.list(), d.list()
	s.Cwd, s.User, s.Hostname = d.text(), d.text(), d.text()
	s.Flags = uint32(d.number(4))
	s.Network.Bridge = d.text()
	copy(s.Network.IP[:], d.take(4))
	s.Network.Prefix = uint8(d.number(1))
	copy(s.Network.Gateway[:], d.take(4))
	s.Network.ExtraHosts, s.Network.DNSNames, s.Mounts = d.list(), d.list(), d.list()
	s.Limits = d.limits()
	s.Caps = d.number(8)
	if len(d.data) > 0 {
		s.Network.Options = d.list()
	}
	if len(d.data) > 0 {
		s.Options = d.list()
	}
	if d.err != nil {
		return RunSpec{}, d.err
	}
	if len(d.data) != 0 {
		return RunSpec{}, fmt.Errorf("trailing bytes in Run")
	}
	var err error
	s.DNS, s.Network.Options, err = decodeDNSOptions(s.Network.Options)
	if err != nil {
		return RunSpec{}, err
	}
	options := s.Network.Options[:0]
	for _, option := range s.Network.Options {
		value, ok := strings.CutPrefix(option, domainnameOption)
		if !ok {
			options = append(options, option)
			continue
		}
		if s.Domainname != "" || value == "" || ValidateDomainname(value) != nil {
			return RunSpec{}, fmt.Errorf("invalid Run domain name")
		}
		s.Domainname = value
	}
	s.Network.Options = options
	for i, option := range s.Options {
		if raw, ok := strings.CutPrefix(option, initialNetworksOption); ok {
			if err := json.Unmarshal([]byte(raw), &s.AdditionalNetworks); err != nil || len(s.AdditionalNetworks) >= MaxEndpoints {
				return RunSpec{}, fmt.Errorf("invalid initial network list")
			}
			s.Options = append(s.Options[:i], s.Options[i+1:]...)
			break
		}
	}
	s.Process, s.Options, err = decodeProcessOptions(s.Options)
	if err != nil {
		return RunSpec{}, err
	}
	s.Devices, s.Options, err = decodeDeviceOptions(s.Options)
	if err != nil {
		return RunSpec{}, err
	}
	s.Sysctls, s.Options, err = decodeSysctlOptions(s.Options, s.PrivateNetwork())
	if err != nil {
		return RunSpec{}, err
	}
	return s, nil
}

func DecodeSignal(f Frame) (string, uint32, error) {
	d := runDecoder{data: f.Payload}
	id := d.text()
	signal := uint32(d.number(4))
	if len(d.data) != 0 && d.err == nil {
		d.err = fmt.Errorf("trailing bytes in Kill")
	}
	return id, signal, d.err
}

func (event RunEvent) Frame() (Frame, error) {
	var e encoder
	e.text(event.ID)
	switch event.Kind {
	case StartedMessage:
		e.u32(event.PID)
	case OutputMessage:
		e.add(event.Stream)
		if len(event.Data) > MaxFrame-headerSize {
			return Frame{}, fmt.Errorf("output frame too large")
		}
		e.u32(uint32(len(event.Data)))
		e.add(event.Data...)
	case ExitedMessage:
		e.u32(uint32(event.Exit.Code))
		e.u64(event.Exit.SetupNS)
		e.u64(event.Exit.RunNS)
		var oom byte
		if event.Exit.OOMKilled {
			oom = 1
		}
		e.add(oom)
	default:
		return Frame{}, fmt.Errorf("invalid run event kind")
	}
	return Frame{Kind: event.Kind, Payload: e.payload}, e.err
}
