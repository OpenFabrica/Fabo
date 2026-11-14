package guestwire

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"unicode/utf8"
)

const (
	RunMessage     Kind = 12
	StartedMessage Kind = 13
	OutputMessage  Kind = 14
	ExitedMessage  Kind = 15
	// SettledMessage tells the host that a container reported started has
	// entered its own root and namespaces, so requests that read them (exec,
	// a network join) can go to the guest. It carries only the ID. The guest
	// never waits for this on its serial control channel; the host does.
	SettledMessage Kind = 81
)

// RunAutoRemove removes the writable layer when the container exits.
const RunAutoRemove uint32 = 1

// RunNetworkNone gives a container its own network namespace with loopback only.
const RunNetworkNone uint32 = 2

// RunTTY combines stdout and stderr on a pseudoterminal.
const RunTTY uint32 = 4

// RunStdin connects the container stdin to guest input messages.
const RunStdin uint32 = 8

// RunNetworkHost shares the Linux guest or host process network namespace.
const RunNetworkHost uint32 = 32

// RunPrivileged selects the privileged capability and syscall policy for
// a container or exec. The guest still enforces its host transport boundary.
const RunPrivileged uint32 = 64

// RunNoSeccomp skips the default policy while retaining the guest transport filter.
const RunNoSeccomp uint32 = 128

// RunNoNewPrivs prohibits acquiring privileges through exec.
const RunNoNewPrivs uint32 = 256

const RunSecurityFlags = RunNoSeccomp | RunNoNewPrivs

// RunInit keeps a small supervisor as PID 1 to reap orphans and forward signals.
const RunInit uint32 = 1 << 16

type RunSpec struct {
	ID                  string
	Layers, Argv, Env   []string
	Cwd, User, Hostname string
	Domainname          string `json:",omitempty"`
	Flags               uint32
	Network             Network
	AdditionalNetworks  []Network
	DNS                 DNSConfig         `json:",omitzero"`
	Process             ProcessOptions    `json:",omitzero"`
	Devices             DeviceOptions     `json:",omitzero"`
	Sysctls             map[string]string `json:",omitempty"`
	Mounts              []string
	Limits              Limits
	Caps                uint64
	Options             []string
}

// DefaultBridge is the protocol name of Fabo’s built-in bridge.
const DefaultBridge = "fabo0"

type Network struct {
	Bridge                        string
	IP, Gateway                   [4]byte
	Prefix                        uint8
	ExtraHosts, DNSNames, Options []string
	StaticIPv4, StaticIPv6        bool `json:",omitzero"`
}

type Limits struct {
	MemoryMax, CPUQuota           uint64
	CPUPeriod, CPUWeight, PIDsMax uint32
	ShmSize                       uint64
}

// Frame encodes the Rust protocol's Run message, including both trailing option lists.
func (s RunSpec) Frame() (Frame, error) {
	var e encoder
	e.text(s.ID)
	e.list(s.Layers)
	e.list(s.Argv)
	e.list(s.Env)
	e.text(s.Cwd)
	e.text(s.User)
	e.text(s.Hostname)
	e.u32(s.Flags)
	e.text(s.Network.Bridge)
	e.add(s.Network.IP[:]...)
	e.add(s.Network.Prefix)
	e.add(s.Network.Gateway[:]...)
	e.list(s.Network.ExtraHosts)
	e.list(s.Network.DNSNames)
	e.list(s.Mounts)
	e.limits(s.Limits)
	e.u64(s.Caps)
	networkOptions := s.Network.Options
	if err := s.DNS.Validate(); err != nil {
		return Frame{}, err
	}
	if err := ValidateDomainname(s.Domainname); err != nil {
		return Frame{}, err
	}
	if !s.DNS.Empty() || s.Domainname != "" {
		networkOptions = s.DNS.appendOptions(slices.Clone(networkOptions))
		if s.Domainname != "" {
			networkOptions = append(networkOptions, domainnameOption+s.Domainname)
		}
	}
	e.list(networkOptions)
	options, err := s.Process.appendOptions(s.Options)
	if err != nil {
		return Frame{}, err
	}
	options, err = s.Devices.appendOptions(options)
	if err != nil {
		return Frame{}, err
	}
	options, err = s.appendSysctlOptions(options)
	if err != nil {
		return Frame{}, err
	}
	if len(s.AdditionalNetworks) > 0 {
		if len(s.AdditionalNetworks) >= MaxEndpoints {
			return Frame{}, fmt.Errorf("too many networks")
		}
		raw, err := json.Marshal(s.AdditionalNetworks)
		if err != nil {
			return Frame{}, err
		}
		options = append(slices.Clone(options), initialNetworksOption+string(raw))
	}
	e.list(options)
	if e.err != nil {
		return Frame{}, e.err
	}
	return Frame{Kind: RunMessage, Payload: e.payload}, nil
}

type encoder struct {
	payload []byte
	err     error
}

func (e *encoder) add(data ...byte) {
	if e.err != nil {
		return
	}
	if len(data) > MaxFrame-headerSize-len(e.payload) {
		e.err = fmt.Errorf("guest frame exceeds %d bytes", MaxFrame)
		return
	}
	e.payload = append(e.payload, data...)
}

func (e *encoder) u32(n uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], n)
	e.add(b[:]...)
}

func (e *encoder) u64(n uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], n)
	e.add(b[:]...)
}

func (e *encoder) text(s string) {
	if e.err != nil {
		return
	}
	if len(s) > MaxFrame-headerSize-4-len(e.payload) {
		e.err = fmt.Errorf("guest frame exceeds %d bytes", MaxFrame)
		return
	}
	if !utf8.ValidString(s) {
		e.err = fmt.Errorf("guest string is not UTF-8")
		return
	}
	e.u32(uint32(len(s)))
	e.payload = append(e.payload, s...)
}

func (e *encoder) list(items []string) {
	if len(items) > (MaxFrame-headerSize)/4 {
		e.err = fmt.Errorf("guest string list cannot fit in a frame")
		return
	}
	e.u32(uint32(len(items)))
	for _, item := range items {
		e.text(item)
		if e.err != nil {
			return
		}
	}
}

type Exit struct {
	Code           int32
	SetupNS, RunNS uint64
	OOMKilled      bool
}

// RunEvent carries one guest lifecycle event. Data borrows the frame buffer.
type RunEvent struct {
	Kind   Kind
	ID     string
	PID    uint32
	Stream byte
	Data   []byte
	Exit   Exit
}

func DecodeRunEvent(frame Frame) (RunEvent, error) {
	id, payload, err := takeBytes(frame.Payload)
	if err != nil {
		return RunEvent{}, err
	}
	if len(id) == 0 || !utf8.Valid(id) {
		return RunEvent{}, fmt.Errorf("invalid container ID in guest event")
	}
	event := RunEvent{Kind: frame.Kind, ID: string(id)}
	switch frame.Kind {
	case StartedMessage:
		if len(payload) != 4 {
			return RunEvent{}, fmt.Errorf("invalid Started payload size %d", len(payload))
		}
		event.PID = binary.LittleEndian.Uint32(payload)
	case SettledMessage:
		if len(payload) != 0 {
			return RunEvent{}, fmt.Errorf("invalid Settled payload size %d", len(payload))
		}
	case OutputMessage:
		if len(payload) == 0 || (payload[0] != 1 && payload[0] != 2) {
			return RunEvent{}, fmt.Errorf("invalid container output stream")
		}
		data, rest, err := takeBytes(payload[1:])
		if err != nil {
			return RunEvent{}, err
		}
		if len(rest) != 0 {
			return RunEvent{}, fmt.Errorf("trailing container output bytes")
		}
		event.Stream, event.Data = payload[0], data
	case ExitedMessage:
		if len(payload) != 20 && len(payload) != 21 {
			return RunEvent{}, fmt.Errorf("invalid Exited payload size %d", len(payload))
		}
		event.Exit = Exit{
			Code:      int32(binary.LittleEndian.Uint32(payload)),
			SetupNS:   binary.LittleEndian.Uint64(payload[4:]),
			RunNS:     binary.LittleEndian.Uint64(payload[12:]),
			OOMKilled: len(payload) == 21 && payload[20] != 0,
		}
	default:
		return RunEvent{}, fmt.Errorf("unexpected guest lifecycle message %d", frame.Kind)
	}
	return event, nil
}

// RunAttached runs one container on an exclusive control stream. Output bytes
// are valid during the synchronous callback. Close the stream after an error.
func RunAttached(rw io.ReadWriter, spec RunSpec, started func(uint32), output func(stream byte, data []byte) error) (Exit, error) {
	request, err := spec.Frame()
	if err != nil {
		return Exit{}, err
	}
	if err := WriteFrame(rw, request); err != nil {
		return Exit{}, err
	}
	frame, err := readReply(rw, StartedMessage)
	if err != nil {
		return Exit{}, err
	}
	for {
		if frame.Kind != MemoryPressure {
			if frame.Kind == ErrorMessage {
				return Exit{}, expect(frame, ExitedMessage)
			}
			event, err := DecodeRunEvent(frame)
			if err != nil {
				return Exit{}, err
			}
			if event.ID != spec.ID {
				return Exit{}, fmt.Errorf("guest event for %q, expected %q", event.ID, spec.ID)
			}
			switch event.Kind {
			case StartedMessage:
				if started != nil {
					started(event.PID)
				}
			case OutputMessage:
				if output != nil {
					if err := output(event.Stream, event.Data); err != nil {
						return Exit{}, err
					}
				}
			case ExitedMessage:
				return event.Exit, nil
			}
		}
		frame, err = ReadFrame(rw, frame.Payload)
		if err != nil {
			return Exit{}, err
		}
	}
}

func takeBytes(payload []byte) (data, rest []byte, err error) {
	if len(payload) < 4 {
		return nil, nil, io.ErrUnexpectedEOF
	}
	n := binary.LittleEndian.Uint32(payload)
	if n > uint32(len(payload)-4) {
		return nil, nil, io.ErrUnexpectedEOF
	}
	return payload[4 : 4+n], payload[4+n:], nil
}
