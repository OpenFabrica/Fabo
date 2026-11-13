package guestwire

import (
	"context"
	"fmt"
	"path"
	"regexp"
)

const (
	VolumeRoot = "/var/lib/kiri/volumes"

	RemoveVolumeMessage Kind = 31 // original fire-and-forget command
	AckMessage          Kind = 43
	DeleteVolumeMessage Kind = 76 // acknowledged deletion for the Go VM
)

var volumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func ValidVolumeName(name string) bool {
	return volumeName.MatchString(name)
}

func VMVolumePath(name string) string {
	return path.Join(VolumeRoot, name, "_data")
}

func VolumeNameAt(source string) (string, bool) {
	name := path.Base(path.Dir(source))
	return name, ValidVolumeName(name) && source == VMVolumePath(name)
}

func DecodeVolumeName(frame Frame) (string, error) {
	data, rest, err := takeBytes(frame.Payload)
	name := string(data)
	if err != nil || len(rest) != 0 || !ValidVolumeName(name) {
		return "", fmt.Errorf("invalid volume name")
	}
	return name, nil
}

// DeleteVolume waits for removal of the named guest volume. Using a
// separate opcode leaves the original one-way RemoveVolume compatible.
func (s *Session) DeleteVolume(ctx context.Context, name string) error {
	var enc encoder
	enc.text(name)
	if enc.err != nil {
		return enc.err
	}
	reply, err := s.request(ctx, Frame{Kind: DeleteVolumeMessage, Payload: enc.payload}, AckMessage, "", nil, 0)
	if err != nil {
		return err
	}
	if len(reply.Payload) != 0 {
		return fmt.Errorf("invalid volume deletion acknowledgment")
	}
	return nil
}
