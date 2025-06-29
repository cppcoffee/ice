// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package ice

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"

	"github.com/pion/logging"
	"github.com/pion/stun/v3"
	"github.com/pion/transport/v3"
	"github.com/pion/transport/v3/stdnet"
)

// UDPMux allows multiple connections to go over a single UDP port.
type UDPMux interface {
	io.Closer
	GetConn(ufrag string, addr net.Addr) (net.PacketConn, error)
	RemoveConnByUfrag(ufrag string)
	GetListenAddresses() []net.Addr
}

// UDPMuxDefault is an implementation of the interface.
type UDPMuxDefault struct {
	params UDPMuxParams

	closedChan chan struct{}
	closeOnce  sync.Once

	// connsIPv4 and connsIPv6 are maps of all udpMuxedConn indexed by ufrag|network|candidateType
	connsIPv4, connsIPv6 map[string]*udpMuxedConn

	addressMapMu sync.RWMutex
	addressMap   map[ipPort]*udpMuxedConn

	// Buffer pool to recycle buffers for net.UDPAddr encodes/decodes
	pool *sync.Pool

	mu sync.Mutex

	// For UDP connection listen at unspecified address
	localAddrsForUnspecified []net.Addr
}

// UDPMuxParams are parameters for UDPMux.
type UDPMuxParams struct {
	Logger        logging.LeveledLogger
	UDPConn       net.PacketConn
	UDPConnString string

	// Required for gathering local addresses
	// in case a un UDPConn is passed which does not
	// bind to a specific local address.
	Net transport.Net
}

// NewUDPMuxDefault creates an implementation of UDPMux.
func NewUDPMuxDefault(params UDPMuxParams) *UDPMuxDefault { //nolint:cyclop
	if params.Logger == nil {
		params.Logger = logging.NewDefaultLoggerFactory().NewLogger("ice")
	}

	var localAddrsForUnspecified []net.Addr
	if udpAddr, ok := params.UDPConn.LocalAddr().(*net.UDPAddr); !ok { //nolint:nestif
		params.Logger.Errorf("LocalAddr is not a net.UDPAddr, got %T", params.UDPConn.LocalAddr())
	} else if ok && udpAddr.IP.IsUnspecified() {
		// For unspecified addresses, the correct behavior is to return errListenUnspecified, but
		// it will break the applications that are already using unspecified UDP connection
		// with UDPMuxDefault, so print a warn log and create a local address list for mux.
		params.Logger.Warn("UDPMuxDefault should not listening on unspecified address, use NewMultiUDPMuxFromPort instead")
		var networks []NetworkType
		switch {
		case udpAddr.IP.To4() != nil:
			networks = []NetworkType{NetworkTypeUDP4}

		case udpAddr.IP.To16() != nil:
			networks = []NetworkType{NetworkTypeUDP4, NetworkTypeUDP6}

		default:
			params.Logger.Errorf("LocalAddr expected IPV4 or IPV6, got %T", params.UDPConn.LocalAddr())
		}
		if len(networks) > 0 {
			if params.Net == nil {
				var err error
				if params.Net, err = stdnet.NewNet(); err != nil {
					params.Logger.Errorf("Failed to get create network: %v", err)
				}
			}

			_, addrs, err := localInterfaces(params.Net, nil, nil, networks, true)
			if err == nil {
				localAddrsForUnspecified = make([]net.Addr, len(addrs))
				for i, addr := range addrs {
					localAddrsForUnspecified[i] = &net.UDPAddr{
						IP:   addr.AsSlice(),
						Port: udpAddr.Port,
						Zone: addr.Zone(),
					}
				}
			} else {
				params.Logger.Errorf("Failed to get local interfaces for unspecified addr: %v", err)
			}
		}
	}
	params.UDPConnString = params.UDPConn.LocalAddr().String()

	mux := &UDPMuxDefault{
		addressMap: map[ipPort]*udpMuxedConn{},
		params:     params,
		connsIPv4:  make(map[string]*udpMuxedConn),
		connsIPv6:  make(map[string]*udpMuxedConn),
		closedChan: make(chan struct{}, 1),
		pool: &sync.Pool{
			New: func() any {
				// Big enough buffer to fit both packet and address
				return newBufferHolder(receiveMTU)
			},
		},
		localAddrsForUnspecified: localAddrsForUnspecified,
	}

	go mux.connWorker()

	return mux
}

// LocalAddr returns the listening address of this UDPMuxDefault.
func (m *UDPMuxDefault) LocalAddr() net.Addr {
	return m.params.UDPConn.LocalAddr()
}

// GetListenAddresses returns the list of addresses that this mux is listening on.
func (m *UDPMuxDefault) GetListenAddresses() []net.Addr {
	if len(m.localAddrsForUnspecified) > 0 {
		return m.localAddrsForUnspecified
	}

	return []net.Addr{m.LocalAddr()}
}

// GetConn returns a PacketConn given the connection's ufrag and network address.
// creates the connection if an existing one can't be found.
func (m *UDPMuxDefault) GetConn(ufrag string, addr net.Addr) (net.PacketConn, error) {
	// don't check addr for mux using unspecified address
	if len(m.localAddrsForUnspecified) == 0 && m.params.UDPConnString != addr.String() {
		return nil, errInvalidAddress
	}

	var isIPv6 bool
	if udpAddr, _ := addr.(*net.UDPAddr); udpAddr != nil && udpAddr.IP.To4() == nil {
		isIPv6 = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.IsClosed() {
		return nil, io.ErrClosedPipe
	}

	if conn, ok := m.getConn(ufrag, isIPv6); ok {
		return conn, nil
	}

	c := m.createMuxedConn(ufrag)
	go func() {
		<-c.CloseChannel()
		m.RemoveConnByUfrag(ufrag)
	}()

	if isIPv6 {
		m.connsIPv6[ufrag] = c
	} else {
		m.connsIPv4[ufrag] = c
	}

	return c, nil
}

// RemoveConnByUfrag stops and removes the muxed packet connection.
func (m *UDPMuxDefault) RemoveConnByUfrag(ufrag string) {
	removedConns := make([]*udpMuxedConn, 0, 2)

	// Keep lock section small to avoid deadlock with conn lock.
	m.mu.Lock()
	if c, ok := m.connsIPv4[ufrag]; ok {
		delete(m.connsIPv4, ufrag)
		removedConns = append(removedConns, c)
	}
	if c, ok := m.connsIPv6[ufrag]; ok {
		delete(m.connsIPv6, ufrag)
		removedConns = append(removedConns, c)
	}
	m.mu.Unlock()

	if len(removedConns) == 0 {
		// No need to lock if no connection was found.
		return
	}

	m.addressMapMu.Lock()
	defer m.addressMapMu.Unlock()

	for _, c := range removedConns {
		addresses := c.getAddresses()
		for _, addr := range addresses {
			delete(m.addressMap, addr)
		}
	}
}

// IsClosed returns true if the mux had been closed.
func (m *UDPMuxDefault) IsClosed() bool {
	select {
	case <-m.closedChan:
		return true
	default:
		return false
	}
}

// Close the mux, no further connections could be created.
func (m *UDPMuxDefault) Close() error {
	var err error
	m.closeOnce.Do(func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		for _, c := range m.connsIPv4 {
			_ = c.Close()
		}
		for _, c := range m.connsIPv6 {
			_ = c.Close()
		}

		m.connsIPv4 = make(map[string]*udpMuxedConn)
		m.connsIPv6 = make(map[string]*udpMuxedConn)

		close(m.closedChan)

		_ = m.params.UDPConn.Close()
	})

	return err
}

func (m *UDPMuxDefault) writeTo(buf []byte, rAddr net.Addr) (n int, err error) {
	return m.params.UDPConn.WriteTo(buf, rAddr)
}

func (m *UDPMuxDefault) registerConnForAddress(conn *udpMuxedConn, addr ipPort) {
	if m.IsClosed() {
		return
	}

	m.addressMapMu.Lock()
	defer m.addressMapMu.Unlock()

	existing, ok := m.addressMap[addr]
	if ok {
		existing.removeAddress(addr)
	}
	m.addressMap[addr] = conn

	m.params.Logger.Debugf("Registered %s for %s", addr.addr.String(), conn.params.Key)
}

func (m *UDPMuxDefault) createMuxedConn(key string) *udpMuxedConn {
	c := newUDPMuxedConn(&udpMuxedConnParams{
		Mux:       m,
		Key:       key,
		AddrPool:  m.pool,
		LocalAddr: m.LocalAddr(),
		Logger:    m.params.Logger,
	})

	return c
}

func (m *UDPMuxDefault) connWorker() { //nolint:cyclop
	logger := m.params.Logger

	defer func() {
		_ = m.Close()
	}()

	buf := make([]byte, receiveMTU)
	for {
		n, addr, err := m.params.UDPConn.ReadFrom(buf)
		if m.IsClosed() {
			return
		} else if err != nil {
			if os.IsTimeout(err) {
				continue
			} else if !errors.Is(err, io.EOF) {
				logger.Errorf("Failed to read UDP packet: %v", err)
			}

			return
		}

		netUDPAddr, ok := addr.(*net.UDPAddr)
		if !ok {
			logger.Errorf("Underlying PacketConn did not return a UDPAddr")

			return
		}
		udpAddr, err := newIPPort(netUDPAddr.IP, netUDPAddr.Zone, uint16(netUDPAddr.Port)) //nolint:gosec
		if err != nil {
			logger.Errorf("Failed to create a new IP/Port host pair")

			return
		}

		// If we have already seen this address dispatch to the appropriate destination
		m.addressMapMu.Lock()
		destinationConn := m.addressMap[udpAddr]
		m.addressMapMu.Unlock()

		// If we haven't seen this address before but is a STUN packet lookup by ufrag
		if destinationConn == nil && stun.IsMessage(buf[:n]) {
			msg := &stun.Message{
				Raw: append([]byte{}, buf[:n]...),
			}

			if err = msg.Decode(); err != nil {
				m.params.Logger.Warnf("Failed to handle decode ICE from %s: %v", addr.String(), err)

				continue
			}

			attr, stunAttrErr := msg.Get(stun.AttrUsername)
			if stunAttrErr != nil {
				m.params.Logger.Warnf("No Username attribute in STUN message from %s", addr.String())

				continue
			}

			ufrag := strings.Split(string(attr), ":")[0]
			isIPv6 := netUDPAddr.IP.To4() == nil

			m.mu.Lock()
			destinationConn, _ = m.getConn(ufrag, isIPv6)
			m.mu.Unlock()
		}

		if destinationConn == nil {
			m.params.Logger.Tracef("Dropping packet from %s, addr: %s", udpAddr.addr, addr)

			continue
		}

		if err = destinationConn.writePacket(buf[:n], netUDPAddr); err != nil {
			m.params.Logger.Errorf("Failed to write packet: %v", err)
		}
	}
}

func (m *UDPMuxDefault) getConn(ufrag string, isIPv6 bool) (val *udpMuxedConn, ok bool) {
	if isIPv6 {
		val, ok = m.connsIPv6[ufrag]
	} else {
		val, ok = m.connsIPv4[ufrag]
	}

	return
}

type bufferHolder struct {
	next *bufferHolder
	buf  []byte
	addr *net.UDPAddr
}

func newBufferHolder(size int) *bufferHolder {
	return &bufferHolder{
		buf: make([]byte, size),
	}
}

func (b *bufferHolder) reset() {
	b.next = nil
	b.addr = nil
}

type ipPort struct {
	addr netip.Addr
	port uint16
}

// newIPPort create a custom type of address based on netip.Addr and
// port. The underlying ip address passed is converted to IPv6 format
// to simplify ip address handling.
func newIPPort(ip net.IP, zone string, port uint16) (ipPort, error) {
	n, ok := netip.AddrFromSlice(ip.To16())
	if !ok {
		return ipPort{}, errInvalidIPAddress
	}

	return ipPort{
		addr: n.WithZone(zone),
		port: port,
	}, nil
}
