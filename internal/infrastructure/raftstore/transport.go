package raftstore

import (
	"crypto/tls"
	"io"
	"net"
	"time"

	"github.com/hashicorp/raft"
)

// tlsStream carries Raft traffic over mutual TLS between cluster members.
type tlsStream struct {
	net.Listener
	client    *tls.Config
	advertise net.Addr
}

func (s *tlsStream) Dial(addr raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	return tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", string(addr), s.client)
}

func (s *tlsStream) Addr() net.Addr { return s.advertise }

// NewTLSTransport listens on bind and advertises advertise (host:port) to peers.
func NewTLSTransport(bind, advertise string, server, client *tls.Config, logOut io.Writer) (*raft.NetworkTransport, error) {
	adv, err := net.ResolveTCPAddr("tcp", advertise)
	if err != nil {
		return nil, err
	}
	l, err := tls.Listen("tcp", bind, server)
	if err != nil {
		return nil, err
	}
	if logOut == nil {
		logOut = io.Discard
	}
	return raft.NewNetworkTransport(&tlsStream{Listener: l, client: client, advertise: adv}, 3, 10*time.Second, logOut), nil
}
