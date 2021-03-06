// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Client implements a traditional SSH client that supports shells,
// subprocesses, TCP port/streamlocal forwarding and tunneled dialing.
type Client struct {
	Conn
	Halt *Halter

	Forwards        ForwardList // forwarded tcpip connections from the remote side
	Mu              sync.Mutex
	ChannelHandlers map[string]chan NewChannel

	TmpCtx context.Context
}

// HandleChannelOpen returns a channel on which NewChannel requests
// for the given type are sent. If the type already is being handled,
// nil is returned. The channel is closed when the connection is closed.
func (c *Client) HandleChannelOpen(channelType string) <-chan NewChannel {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	if c.ChannelHandlers == nil {
		// The SSH channel has been closed.
		c := make(chan NewChannel)
		close(c)
		return c
	}

	ch := c.ChannelHandlers[channelType]
	if ch != nil {
		return nil
	}

	ch = make(chan NewChannel, chanSize)
	c.ChannelHandlers[channelType] = ch
	return ch
}

// NewClient creates a Client on top of the given connection.
func NewClient(ctx context.Context, c Conn, chans <-chan NewChannel, reqs <-chan *Request, halt *Halter) *Client {
	conn := &Client{
		Conn:            c,
		ChannelHandlers: make(map[string]chan NewChannel, 1),
		Halt:            halt,
	}

	go conn.HandleGlobalRequests(ctx, reqs)
	go conn.HandleChannelOpens(ctx, chans)
	go func() {
		conn.Wait()
		conn.Forwards.CloseAll()
	}()
	go conn.Forwards.HandleChannels(ctx, conn.HandleChannelOpen("forwarded-tcpip"), c)
	go conn.Forwards.HandleChannels(ctx, conn.HandleChannelOpen("forwarded-streamlocal@openssh.com"), c)
	return conn
}

// NewClientConn establishes an authenticated SSH connection using c
// as the underlying transport.  The Request and NewChannel channels
// must be serviced or the connection will hang.
func NewClientConn(ctx context.Context, c net.Conn, addr string, config *ClientConfig) (Conn, <-chan NewChannel, <-chan *Request, error) {
	fullConf := *config
	fullConf.SetDefaults()
	if fullConf.HostKeyCallback == nil {
		c.Close()
		return nil, nil, nil, errors.New("ssh: must specify HostKeyCallback")
	}
	if fullConf.Halt == nil {
		c.Close()
		return nil, nil, nil, errors.New("ssh: config must provide Halt")
	}
	conn := newConnection(c, &fullConf.Config, &fullConf)

	// can block on conn here, we need to get a close
	// on conn in.
	if err := conn.clientHandshake(ctx, addr, &fullConf); err != nil {
		c.Close()
		return nil, nil, nil, fmt.Errorf("ssh: handshake failed: %v", err)
	}

	conn.mux = newMux(ctx, conn.transport, conn.halt)
	return conn, conn.mux.incomingChannels, conn.mux.incomingRequests, nil
}

// clientHandshake performs the client side key exchange. See RFC 4253 Section
// 7.
func (c *connection) clientHandshake(ctx context.Context, dialAddress string, config *ClientConfig) error {
	if config.ClientVersion != "" {
		c.clientVersion = []byte(config.ClientVersion)
	} else {
		c.clientVersion = []byte(packageVersion)
	}
	var err error
	c.serverVersion, err = exchangeVersions(c.sshConn.conn, c.clientVersion)
	if err != nil {
		return err
	}

	c.transport = newClientTransport(ctx,
		newTransport(c.sshConn.conn, config.Rand, true /* is client */, &config.Config),
		c.clientVersion, c.serverVersion, config, dialAddress, c.sshConn.RemoteAddr())
	if c.transport == nil {
		return ErrShutDown
	}
	if err := c.transport.waitSession(ctx); err != nil {
		return err
	}

	c.sessionID = c.transport.getSessionID()
	return c.clientAuthenticate(ctx, config)
}

// verifyHostKeySignature verifies the host key obtained in the key
// exchange.
func verifyHostKeySignature(hostKey PublicKey, result *kexResult) error {
	sig, rest, ok := parseSignatureBody(result.Signature)
	if len(rest) > 0 || !ok {
		return errors.New("ssh: signature parse error")
	}

	return hostKey.Verify(result.H, sig)
}

// NewSession opens a new Session for this client. (A session is a remote
// execution of a program.)
func (c *Client) NewSession(ctx context.Context) (*Session, error) {
	ch, in, err := c.OpenChannel(ctx, "session", nil, nil)
	if err != nil {
		return nil, err
	}
	return newSession(ch, in)
}

func (c *Client) HandleGlobalRequests(ctx context.Context, incoming <-chan *Request) {

	for {
		select {
		case r := <-incoming:
			if r != nil {
				// This handles keepalive messages and matches
				// the behaviour of OpenSSH.
				r.Reply(false, nil)
			}
		case <-c.Halt.ReqStopChan():
			return
		case <-c.Conn.Done():
			return
		case <-ctx.Done():
			return
		}
	}
}

// handleChannelOpens channel open messages from the remote side.
func (c *Client) HandleChannelOpens(ctx context.Context, in <-chan NewChannel) {

	for {
		select {
		case <-c.Halt.ReqStopChan():
			return
		case <-c.Conn.Done():
			return
		case <-ctx.Done():
			return
		case ch := <-in:
			if ch != nil {
				c.Mu.Lock()
				handler := c.ChannelHandlers[ch.ChannelType()]
				c.Mu.Unlock()
				if handler != nil {
					select {
					case handler <- ch:
					case <-c.Halt.ReqStopChan():
						return
					case <-c.Conn.Done():
						return
					case <-ctx.Done():
						return
					}
				} else {
					ch.Reject(UnknownChannelType, fmt.Sprintf("unknown channel type: %v", ch.ChannelType()))
				}
			}
		}
	}
	c.Mu.Lock()
	for _, ch := range c.ChannelHandlers {
		close(ch)
	}
	c.ChannelHandlers = nil
	c.Mu.Unlock()
}

// Dial starts a client connection to the given SSH server. It is a
// convenience function that connects to the given network address,
// initiates the SSH handshake, and then sets up a Client.  For access
// to incoming channels and requests, use net.Dial with NewClientConn
// instead.
func Dial(ctx context.Context, network, addr string, config *ClientConfig) (*Client, error) {
	conn, err := net.DialTimeout(network, addr, config.Timeout)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := NewClientConn(ctx, conn, addr, config)
	if err != nil {
		return nil, err
	}
	return NewClient(ctx, c, chans, reqs, config.Halt), nil
}

// HostKeyCallback is the function type used for verifying server
// keys.  A HostKeyCallback must return nil if the host key is OK, or
// an error to reject it. It receives the hostname as passed to Dial
// or NewClientConn. The remote address is the RemoteAddr of the
// net.Conn underlying the the SSH connection.
type HostKeyCallback func(hostname string, remote net.Addr, key PublicKey) error

// A ClientConfig structure is used to configure a Client. It must not be
// modified after having been passed to an SSH function.
type ClientConfig struct {
	// Config contains configuration that is shared between clients and
	// servers.
	Config

	// User contains the username to authenticate as.
	User string

	// HostPort has the IP:port in string form.
	HostPort string

	// Auth contains possible authentication methods to use with the
	// server. Only the first instance of a particular RFC 4252 method will
	// be used during authentication.
	Auth []AuthMethod

	// HostKeyCallback is called during the cryptographic
	// handshake to validate the server's host key. The client
	// configuration must supply this callback for the connection
	// to succeed. The functions InsecureIgnoreHostKey or
	// FixedHostKey can be used for simplistic host key checks.
	HostKeyCallback HostKeyCallback

	// ClientVersion contains the version identification string that will
	// be used for the connection. If empty, a reasonable default is used.
	ClientVersion string

	// HostKeyAlgorithms lists the key types that the client will
	// accept from the server as host key, in order of
	// preference. If empty, a reasonable default is used. Any
	// string returned from PublicKey.Type method may be used, or
	// any of the CertAlgoXxxx and KeyAlgoXxxx constants.
	HostKeyAlgorithms []string

	// Timeout is the maximum amount of time for the TCP connection to establish.
	//
	// A Timeout of zero means no timeout.
	Timeout time.Duration
}

// InsecureIgnoreHostKey returns a function that can be used for
// ClientConfig.HostKeyCallback to accept any host key. It should
// not be used for production code.
func InsecureIgnoreHostKey() HostKeyCallback {
	return func(hostname string, remote net.Addr, key PublicKey) error {
		return nil
	}
}

type fixedHostKey struct {
	key PublicKey
}

func (f *fixedHostKey) check(hostname string, remote net.Addr, key PublicKey) error {
	if f.key == nil {
		return fmt.Errorf("ssh: required host key was nil")
	}
	if !bytes.Equal(key.Marshal(), f.key.Marshal()) {
		return fmt.Errorf("ssh: host key mismatch")
	}
	return nil
}

// FixedHostKey returns a function for use in
// ClientConfig.HostKeyCallback to accept only a specific host key.
func FixedHostKey(key PublicKey) HostKeyCallback {
	hk := &fixedHostKey{key}
	return hk.check
}
