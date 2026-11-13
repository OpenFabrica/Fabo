// Package guestagent serves the Linux side of Kiri's guest protocol.
package guestagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

// Serve owns the stream after connection and returns on Shutdown or EOF.
func Serve(conn io.ReadWriteCloser, startedNS uint64, layers *LayerStore, runtime *Runtime, clock func(time.Time) error, rosetta bool) error {
	if runtime != nil {
		defer func() { _ = conn.Close(); runtime.Close() }()
	}
	var writer sync.Mutex
	write := func(frame guestwire.Frame) error {
		writer.Lock()
		defer writer.Unlock()
		return guestwire.WriteFrame(conn, frame)
	}
	emit := func(event guestwire.RunEvent) error {
		frame, err := event.Frame()
		if err != nil {
			return err
		}
		return write(frame)
	}
	ack := func(id string, size uint32) error {
		frame, err := guestwire.InputConsumed(id, size)
		if err != nil {
			return err
		}
		return write(frame)
	}
	initial, err := sample()
	if err != nil {
		return err
	}
	hello := guestwire.Hello{
		Version: guestwire.Version, InitStartNS: startedNS, ReadyNS: initial.UptimeNS,
		MemoryTotalKiB: initial.MemoryTotalKiB, MemoryAvailableKiB: initial.MemoryAvailableKiB,
		Rosetta: rosetta,
	}
	if err := write(hello.Frame()); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var measuring atomic.Bool
	var scratch []byte
	for {
		frame, err := guestwire.ReadFrame(conn, scratch)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		scratch = frame.Payload
		var reply guestwire.Frame
		switch {
		case frame.Kind == guestwire.Ping && len(frame.Payload) == 8:
			reply = guestwire.Frame{Kind: guestwire.Pong, Payload: frame.Payload}
		case frame.Kind == guestwire.StatsRequest && len(frame.Payload) == 0:
			stats, err := sample()
			if err != nil {
				reply = guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte(err.Error())}
			} else {
				reply = stats.Frame()
			}
		case frame.Kind == guestwire.UsageQueryMessage && len(frame.Payload) == 0:
			if measuring.CompareAndSwap(false, true) {
				go func() {
					lines := measureUsage(ctx, layers, runtime)
					if ctx.Err() != nil {
						return
					}
					frame, err := guestwire.UsageFrame(lines)
					if err != nil {
						frame, _ = guestwire.UsageFrame(nil)
					}
					// Clear before sending so a query sent after receipt
					// cannot observe the previous scan as in progress.
					measuring.Store(false)
					_ = write(frame)
				}()
			}
			continue
		case frame.Kind == guestwire.LayerQueryMessage:
			raw, err := guestwire.DecodeLayerQuery(frame)
			if err != nil {
				return err
			}
			digest, err := layerDigest(raw)
			var have byte
			if layers != nil && err == nil && layers.has(digest) {
				have = 1
			}
			reply = guestwire.Frame{Kind: guestwire.LayerStatusMessage, Payload: []byte{have}}
		case frame.Kind == guestwire.LayerBeginMessage:
			begin, err := guestwire.DecodeLayerBegin(frame)
			if err != nil {
				return err
			}
			if layers == nil || begin.Size > math.MaxInt64 {
				return fmt.Errorf("cannot receive layer")
			}
			stored, err := layers.receive(conn, begin)
			if err != nil {
				reply = guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte(err.Error())}
			} else {
				reply = stored.Frame()
			}
		case layers != nil && frame.Kind == guestwire.PruneLayersMessage:
			keep, err := guestwire.DecodePruneLayers(frame)
			if err == nil {
				err = layers.prune(keep)
			}
			if err == nil {
				reply = guestwire.Frame{Kind: guestwire.AckMessage}
			} else {
				reply = guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte(err.Error())}
			}
		case runtime != nil && frame.Kind == guestwire.RunMessage:
			spec, err := guestwire.DecodeRun(frame)
			if err == nil {
				err = runtime.Run(spec, emit, ack)
			}
			if err == nil {
				continue
			}
			reply = guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte(err.Error())}
		case runtime != nil && frame.Kind == guestwire.ExecMessage:
			spec, err := guestwire.DecodeExec(frame)
			if err == nil {
				err = runtime.Exec(spec, emit, ack)
			}
			if err == nil {
				continue
			}
			reply = guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte(err.Error())}
		case runtime != nil && frame.Kind == guestwire.ExecOutputCreditMessage:
			id, count, err := guestwire.DecodeExecOutputCredit(frame)
			if err != nil {
				return err
			}
			runtime.execOutputCredit(id, count)
			continue
		case runtime != nil && frame.Kind == guestwire.InputMessage:
			id, data, err := guestwire.DecodeInput(frame)
			if err != nil {
				return err
			}
			if input := runtime.input(id); input != nil {
				if err := input.enqueue(data); err != nil {
					return err
				}
			}
			continue
		case runtime != nil && frame.Kind == guestwire.CloseStdinMessage:
			id, err := guestwire.DecodeCloseStdin(frame)
			if err != nil {
				return err
			}
			runtime.closeStdin(id)
			continue
		case runtime != nil && frame.Kind == guestwire.ResizeMessage:
			id, rows, cols, err := guestwire.DecodeResize(frame)
			if err != nil {
				return err
			}
			runtime.resize(id, rows, cols)
			continue
		case runtime != nil && frame.Kind == guestwire.KillMessage:
			id, signal, err := guestwire.DecodeSignal(frame)
			if err != nil {
				return err
			}
			runtime.Signal(id, signal)
			continue
		case runtime != nil && frame.Kind == guestwire.UpdateMessage:
			id, limits, err := guestwire.DecodeUpdate(frame)
			if err == nil {
				err = runtime.updateLimits(id, limits)
			}
			if err == nil {
				reply = guestwire.Frame{Kind: guestwire.AckMessage}
			} else {
				reply = guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte(err.Error())}
			}
		case runtime != nil && frame.Kind == guestwire.FreezeMessage:
			id, frozen, err := guestwire.DecodeFreeze(frame)
			if err == nil {
				err = runtime.freeze(id, frozen)
			}
			if err == nil {
				reply = guestwire.Frame{Kind: guestwire.AckMessage}
			} else {
				reply = guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte(err.Error())}
			}
		case runtime != nil && frame.Kind == guestwire.StatsQueryMessage:
			id, err := guestwire.DecodeContainerQuery(frame)
			var stats guestwire.ContainerStats
			if err == nil {
				stats, err = runtime.containerStats(id)
			}
			if err == nil {
				reply = stats.Frame()
			} else {
				reply = guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte(err.Error())}
			}
		case runtime != nil && frame.Kind == guestwire.TopQueryMessage:
			id, err := guestwire.DecodeContainerQuery(frame)
			if err == nil {
				reply, err = runtime.top(id)
			}
			if err != nil {
				reply = guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte(err.Error())}
			}
		case runtime != nil && (frame.Kind == guestwire.FileStatRequest || frame.Kind == guestwire.FileListRequest || frame.Kind == guestwire.FileReadRequest):
			reply, err = runtime.fileQuery(frame)
			if err != nil {
				reply = guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte(err.Error())}
			}
		case runtime != nil && frame.Kind == guestwire.ListenQueryMessage:
			id, err := guestwire.DecodeContainerQuery(frame)
			if err == nil {
				reply, err = runtime.listening(id)
			}
			if err != nil {
				reply = guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte(err.Error())}
			}
		case runtime != nil && (frame.Kind == guestwire.NetAttachMessage || frame.Kind == guestwire.NetDetachMessage):
			id, ifname, network, err := guestwire.DecodeNetworkChange(frame)
			if err == nil {
				err = runtime.changeNetwork(id, ifname, network, frame.Kind == guestwire.NetAttachMessage)
			}
			if err == nil {
				reply = guestwire.Frame{Kind: guestwire.AckMessage}
			} else {
				reply = guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte(err.Error())}
			}
		case runtime != nil && frame.Kind == guestwire.DeleteBridgeMessage:
			name, err := guestwire.DecodeDeleteBridge(frame)
			if err == nil {
				err = runtime.removeBridge(name)
			}
			if err == nil {
				reply = guestwire.Frame{Kind: guestwire.AckMessage}
			} else {
				reply = guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte(err.Error())}
			}
		case runtime != nil && frame.Kind == guestwire.KubernetesRoutesMessage:
			id, err := guestwire.DecodeContainerQuery(frame)
			if err == nil {
				err = runtime.routeKubernetes(id)
			}
			if err == nil {
				reply = guestwire.Frame{Kind: guestwire.AckMessage}
			} else {
				reply = guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte(err.Error())}
			}
		case runtime != nil && frame.Kind == guestwire.RemoveMessage:
			id, err := guestwire.DecodeLayerQuery(frame)
			if err != nil {
				return err
			}
			if err := runtime.Remove(id); err != nil {
				return err
			}
			continue
		case runtime != nil && (frame.Kind == guestwire.RemoveVolumeMessage || frame.Kind == guestwire.DeleteVolumeMessage):
			name, err := guestwire.DecodeVolumeName(frame)
			if err == nil {
				err = runtime.deleteVolume(name)
			}
			if frame.Kind == guestwire.RemoveVolumeMessage {
				if err != nil {
					log.Printf("remove volume: %v", err)
				}
				continue
			}
			if err != nil {
				reply = guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte(err.Error())}
			} else {
				reply = guestwire.Frame{Kind: guestwire.AckMessage}
			}
		case frame.Kind == guestwire.SetTimeMessage:
			now, err := guestwire.DecodeSetTime(frame)
			if err != nil {
				return err
			}
			// A Linux host-process guest shares the host's clock.
			if clock != nil {
				if err := clock(now); err != nil {
					return fmt.Errorf("set VM clock: %w", err)
				}
			}
			continue
		case frame.Kind == guestwire.Shutdown && len(frame.Payload) == 0:
			return nil
		default:
			message := fmt.Sprintf("unsupported or invalid guest message %d", frame.Kind)
			reply = guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte(message)}
		}
		if err := write(reply); err != nil {
			return err
		}
	}
}
