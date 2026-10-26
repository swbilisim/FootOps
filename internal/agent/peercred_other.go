//go:build !linux

package agent

import (
	"errors"
	"net"
)

// The agent only runs on Linux; elsewhere every peer is refused.
func peerUID(net.Conn) (uint32, error) {
	return 0, errors.New("peer credentials are only supported on Linux")
}
