package guestwire

import (
	"context"
	"fmt"
)

const (
	StatsQueryMessage     Kind = 37
	ContainerStatsMessage Kind = 38
)

// ContainerStats is the protocol-v4 sample from a container's Linux cgroup
// and network namespace. SystemTicks counts aggregate Linux USER_HZ ticks.
type ContainerStats struct {
	ReadNS, MemoryCurrent, MemoryMax, MemoryInactiveFile  uint64
	CPUUsageUsec, CPUUserUsec, CPUSystemUsec, SystemTicks uint64
	OnlineCPUs, PIDs                                      uint32
	RXBytes, RXPackets, TXBytes, TXPackets                uint64
	IOReadBytes, IOWriteBytes                             uint64
}

func (s *Session) ContainerStats(ctx context.Context, id string) (ContainerStats, error) {
	var e encoder
	e.text(id)
	if e.err != nil {
		return ContainerStats{}, e.err
	}
	frame, err := s.request(ctx, Frame{Kind: StatsQueryMessage, Payload: e.payload}, ContainerStatsMessage, "", nil, 0)
	if err != nil {
		return ContainerStats{}, err
	}
	return decodeContainerStats(frame)
}

// DecodeContainerQuery reads the sole container ID in a stats or top request.
func DecodeContainerQuery(frame Frame) (string, error) {
	d := runDecoder{data: frame.Payload}
	id := d.text()
	if d.err != nil || len(d.data) != 0 || id == "" {
		return "", fmt.Errorf("invalid container query")
	}
	return id, nil
}

func (s ContainerStats) Frame() Frame {
	var e encoder
	for _, v := range []uint64{s.ReadNS, s.MemoryCurrent, s.MemoryMax, s.MemoryInactiveFile,
		s.CPUUsageUsec, s.CPUUserUsec, s.CPUSystemUsec, s.SystemTicks} {
		e.u64(v)
	}
	e.u32(s.OnlineCPUs)
	e.u32(s.PIDs)
	for _, v := range []uint64{s.RXBytes, s.RXPackets, s.TXBytes, s.TXPackets, s.IOReadBytes, s.IOWriteBytes} {
		e.u64(v)
	}
	return Frame{Kind: ContainerStatsMessage, Payload: e.payload}
}

func decodeContainerStats(frame Frame) (ContainerStats, error) {
	d := runDecoder{data: frame.Payload}
	var s ContainerStats
	for _, v := range []*uint64{&s.ReadNS, &s.MemoryCurrent, &s.MemoryMax, &s.MemoryInactiveFile,
		&s.CPUUsageUsec, &s.CPUUserUsec, &s.CPUSystemUsec, &s.SystemTicks} {
		*v = d.number(8)
	}
	s.OnlineCPUs, s.PIDs = uint32(d.number(4)), uint32(d.number(4))
	for _, v := range []*uint64{&s.RXBytes, &s.RXPackets, &s.TXBytes, &s.TXPackets, &s.IOReadBytes, &s.IOWriteBytes} {
		*v = d.number(8)
	}
	if d.err != nil || len(d.data) != 0 {
		return ContainerStats{}, fmt.Errorf("invalid container stats reply")
	}
	return s, nil
}
