package inbound

import (
	"context"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/proxyproto"
	"github.com/sagernet/sing-box/common/settings"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-dns"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/buf"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/database64128/tfo-go"
)

var _ adapter.Inbound = (*myInboundAdapter)(nil)

type myInboundAdapter struct {
	protocol         string
	network          []string
	ctx              context.Context
	router           adapter.Router
	logger           log.ContextLogger
	tag              string
	listenOptions    option.ListenOptions
	connHandler      adapter.ConnectionHandler
	packetHandler    adapter.PacketHandler
	oobPacketHandler adapter.OOBPacketHandler
	packetUpstream   any

	// http mixed

	setSystemProxy   bool
	clearSystemProxy func() error

	// internal

	tcpListener          net.Listener
	udpConn              *net.UDPConn
	udpAddr              M.Socksaddr
	packetAccess         sync.RWMutex
	packetOutboundClosed chan struct{}
	packetOutbound       chan *myInboundPacket
}

func (a *myInboundAdapter) Type() string {
	return a.protocol
}

func (a *myInboundAdapter) Tag() string {
	return a.tag
}

func (a *myInboundAdapter) Start() error {
	var err error
	if common.Contains(a.network, N.NetworkTCP) {
		_, err = a.ListenTCP()
		if err != nil {
			return err
		}
		go a.loopTCPIn()
	}
	if common.Contains(a.network, N.NetworkUDP) {
		_, err = a.ListenUDP()
		if err != nil {
			return err
		}
		a.packetOutboundClosed = make(chan struct{})
		a.packetOutbound = make(chan *myInboundPacket)
		if a.oobPacketHandler != nil {
			if _, threadUnsafeHandler := common.Cast[N.ThreadUnsafeWriter](a.packetUpstream); !threadUnsafeHandler {
				go a.loopUDPOOBIn()
			} else {
				go a.loopUDPOOBInThreadSafe()
			}
		} else {
			if _, threadUnsafeHandler := common.Cast[N.ThreadUnsafeWriter](a.packetUpstream); !threadUnsafeHandler {
				go a.loopUDPIn()
			} else {
				go a.loopUDPInThreadSafe()
			}
			go a.loopUDPOut()
		}
	}
	if a.setSystemProxy {
		a.clearSystemProxy, err = settings.SetSystemProxy(a.router, M.SocksaddrFromNet(a.tcpListener.Addr()).Port, a.protocol == C.TypeMixed)
		if err != nil {
			return E.Cause(err, "set system proxy")
		}
	}
	return nil
}

func (a *myInboundAdapter) ListenTCP() (net.Listener, error) {
	var err error
	bindAddr := M.SocksaddrFrom(netip.Addr(a.listenOptions.Listen), a.listenOptions.ListenPort)
	var tcpListener net.Listener
	if !a.listenOptions.TCPFastOpen {
		tcpListener, err = net.ListenTCP(M.NetworkFromNetAddr(N.NetworkTCP, bindAddr.Addr), bindAddr.TCPAddr())
	} else {
		tcpListener, err = tfo.ListenTCP(M.NetworkFromNetAddr(N.NetworkTCP, bindAddr.Addr), bindAddr.TCPAddr())
	}
	if err == nil {
		a.logger.Info("tcp server started at ", tcpListener.Addr())
	}
	if a.listenOptions.ProxyProtocol {
		a.logger.Debug("proxy protocol enabled")
		tcpListener = &proxyproto.Listener{Listener: tcpListener}
	}
	a.tcpListener = tcpListener
	return tcpListener, err
}

func (a *myInboundAdapter) ListenUDP() (net.PacketConn, error) {
	bindAddr := M.SocksaddrFrom(netip.Addr(a.listenOptions.Listen), a.listenOptions.ListenPort)
	udpConn, err := net.ListenUDP(M.NetworkFromNetAddr(N.NetworkUDP, bindAddr.Addr), bindAddr.UDPAddr())
	if err != nil {
		return nil, err
	}
	a.udpConn = udpConn
	a.udpAddr = bindAddr
	a.logger.Info("udp server started at ", udpConn.LocalAddr())
	return udpConn, err
}

func (a *myInboundAdapter) Close() error {
	var err error
	if a.clearSystemProxy != nil {
		err = a.clearSystemProxy()
	}
	return E.Errors(err, common.Close(
		a.tcpListener,
		common.PtrOrNil(a.udpConn),
	))
}

func (a *myInboundAdapter) upstreamHandler(metadata adapter.InboundContext) adapter.UpstreamHandlerAdapter {
	return adapter.NewUpstreamHandler(metadata, a.newConnection, a.streamPacketConnection, a)
}

func (a *myInboundAdapter) upstreamContextHandler() adapter.UpstreamHandlerAdapter {
	return adapter.NewUpstreamContextHandler(a.newConnection, a.newPacketConnection, a)
}

func (a *myInboundAdapter) newConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext) error {
	a.logger.InfoContext(ctx, "inbound connection to ", metadata.Destination)
	return a.router.RouteConnection(ctx, conn, metadata)
}

func (a *myInboundAdapter) streamPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext) error {
	a.logger.InfoContext(ctx, "inbound packet connection to ", metadata.Destination)
	return a.router.RoutePacketConnection(ctx, conn, metadata)
}

func (a *myInboundAdapter) newPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext) error {
	ctx = log.ContextWithNewID(ctx)
	a.logger.InfoContext(ctx, "inbound packet connection from ", metadata.Source)
	a.logger.InfoContext(ctx, "inbound packet connection to ", metadata.Destination)
	return a.router.RoutePacketConnection(ctx, conn, metadata)
}

func (a *myInboundAdapter) loopTCPIn() {
	tcpListener := a.tcpListener
	for {
		conn, err := tcpListener.Accept()
		if err != nil {
			return
		}
		go a.injectTCP(conn)
	}
}

func (a *myInboundAdapter) createMetadata(conn net.Conn, metadata adapter.InboundContext) adapter.InboundContext {
	metadata.Inbound = a.tag
	metadata.InboundType = a.protocol
	metadata.SniffEnabled = a.listenOptions.SniffEnabled
	metadata.SniffOverrideDestination = a.listenOptions.SniffOverrideDestination
	metadata.DomainStrategy = dns.DomainStrategy(a.listenOptions.DomainStrategy)
	if !metadata.Source.IsValid() {
		metadata.Source = M.SocksaddrFromNet(conn.RemoteAddr())
	}
	if !metadata.Destination.IsValid() {
		metadata.Destination = M.SocksaddrFromNet(conn.LocalAddr())
	}
	if tcpConn, isTCP := common.Cast[*net.TCPConn](conn); isTCP {
		metadata.OriginDestination = M.SocksaddrFromNet(tcpConn.LocalAddr())
	}
	return metadata
}

func (a *myInboundAdapter) injectTCP(conn net.Conn) {
	ctx := log.ContextWithNewID(a.ctx)
	metadata := a.createMetadata(conn, adapter.InboundContext{})
	a.logger.InfoContext(ctx, "inbound connection from ", metadata.Source)
	hErr := a.connHandler.NewConnection(ctx, conn, metadata)
	if hErr != nil {
		conn.Close()
		a.NewError(ctx, E.Cause(hErr, "process connection from ", metadata.Source))
	}
}

func (a *myInboundAdapter) routeTCP(ctx context.Context, conn net.Conn, metadata adapter.InboundContext) {
	a.logger.InfoContext(ctx, "inbound connection from ", metadata.Source)
	hErr := a.newConnection(ctx, conn, metadata)
	if hErr != nil {
		conn.Close()
		a.NewError(ctx, E.Cause(hErr, "process connection from ", metadata.Source))
	}
}

func (a *myInboundAdapter) loopUDPIn() {
	defer close(a.packetOutboundClosed)
	_buffer := buf.StackNewPacket()
	defer common.KeepAlive(_buffer)
	buffer := common.Dup(_buffer)
	defer buffer.Release()
	buffer.IncRef()
	defer buffer.DecRef()
	packetService := (*myInboundPacketAdapter)(a)
	for {
		buffer.Reset()
		n, addr, err := a.udpConn.ReadFromUDPAddrPort(buffer.FreeBytes())
		if err != nil {
			return
		}
		buffer.Truncate(n)
		var metadata adapter.InboundContext
		metadata.Inbound = a.tag
		metadata.InboundType = a.protocol
		metadata.SniffEnabled = a.listenOptions.SniffEnabled
		metadata.SniffOverrideDestination = a.listenOptions.SniffOverrideDestination
		metadata.DomainStrategy = dns.DomainStrategy(a.listenOptions.DomainStrategy)
		metadata.Source = M.SocksaddrFromNetIP(addr)
		metadata.OriginDestination = a.udpAddr
		err = a.packetHandler.NewPacket(a.ctx, packetService, buffer, metadata)
		if err != nil {
			a.newError(E.Cause(err, "process packet from ", metadata.Source))
		}
	}
}

func (a *myInboundAdapter) loopUDPOOBIn() {
	defer close(a.packetOutboundClosed)
	_buffer := buf.StackNewPacket()
	defer common.KeepAlive(_buffer)
	buffer := common.Dup(_buffer)
	defer buffer.Release()
	buffer.IncRef()
	defer buffer.DecRef()
	packetService := (*myInboundPacketAdapter)(a)
	oob := make([]byte, 1024)
	for {
		buffer.Reset()
		n, oobN, _, addr, err := a.udpConn.ReadMsgUDPAddrPort(buffer.FreeBytes(), oob)
		if err != nil {
			return
		}
		buffer.Truncate(n)
		var metadata adapter.InboundContext
		metadata.Inbound = a.tag
		metadata.InboundType = a.protocol
		metadata.SniffEnabled = a.listenOptions.SniffEnabled
		metadata.SniffOverrideDestination = a.listenOptions.SniffOverrideDestination
		metadata.DomainStrategy = dns.DomainStrategy(a.listenOptions.DomainStrategy)
		metadata.Source = M.SocksaddrFromNetIP(addr)
		metadata.OriginDestination = a.udpAddr
		err = a.oobPacketHandler.NewPacket(a.ctx, packetService, buffer, oob[:oobN], metadata)
		if err != nil {
			a.newError(E.Cause(err, "process packet from ", metadata.Source))
		}
	}
}

func (a *myInboundAdapter) loopUDPInThreadSafe() {
	defer close(a.packetOutboundClosed)
	packetService := (*myInboundPacketAdapter)(a)
	for {
		buffer := buf.NewPacket()
		n, addr, err := a.udpConn.ReadFromUDPAddrPort(buffer.FreeBytes())
		if err != nil {
			buffer.Release()
			return
		}
		buffer.Truncate(n)
		var metadata adapter.InboundContext
		metadata.Inbound = a.tag
		metadata.InboundType = a.protocol
		metadata.SniffEnabled = a.listenOptions.SniffEnabled
		metadata.SniffOverrideDestination = a.listenOptions.SniffOverrideDestination
		metadata.DomainStrategy = dns.DomainStrategy(a.listenOptions.DomainStrategy)
		metadata.Source = M.SocksaddrFromNetIP(addr)
		metadata.OriginDestination = a.udpAddr
		err = a.packetHandler.NewPacket(a.ctx, packetService, buffer, metadata)
		if err != nil {
			buffer.Release()
			a.newError(E.Cause(err, "process packet from ", metadata.Source))
		}
	}
}

func (a *myInboundAdapter) loopUDPOOBInThreadSafe() {
	defer close(a.packetOutboundClosed)
	packetService := (*myInboundPacketAdapter)(a)
	oob := make([]byte, 1024)
	for {
		buffer := buf.NewPacket()
		n, oobN, _, addr, err := a.udpConn.ReadMsgUDPAddrPort(buffer.FreeBytes(), oob)
		if err != nil {
			buffer.Release()
			return
		}
		buffer.Truncate(n)
		var metadata adapter.InboundContext
		metadata.Inbound = a.tag
		metadata.InboundType = a.protocol
		metadata.SniffEnabled = a.listenOptions.SniffEnabled
		metadata.SniffOverrideDestination = a.listenOptions.SniffOverrideDestination
		metadata.DomainStrategy = dns.DomainStrategy(a.listenOptions.DomainStrategy)
		metadata.Source = M.SocksaddrFromNetIP(addr)
		metadata.OriginDestination = a.udpAddr
		err = a.oobPacketHandler.NewPacket(a.ctx, packetService, buffer, oob[:oobN], metadata)
		if err != nil {
			buffer.Release()
			a.newError(E.Cause(err, "process packet from ", metadata.Source))
		}
	}
}

func (a *myInboundAdapter) loopUDPOut() {
	for {
		select {
		case packet := <-a.packetOutbound:
			err := a.writePacket(packet.buffer, packet.destination)
			if err != nil && !E.IsClosed(err) {
				a.newError(E.New("write back udp: ", err))
			}
			continue
		case <-a.packetOutboundClosed:
		}
		for {
			select {
			case packet := <-a.packetOutbound:
				packet.buffer.Release()
			default:
				return
			}
		}
	}
}

func (a *myInboundAdapter) newError(err error) {
	a.logger.Error(err)
}

func (a *myInboundAdapter) NewError(ctx context.Context, err error) {
	NewError(a.logger, ctx, err)
}

func NewError(logger log.ContextLogger, ctx context.Context, err error) {
	common.Close(err)
	if E.IsClosedOrCanceled(err) {
		logger.DebugContext(ctx, "connection closed: ", err)
		return
	}
	logger.ErrorContext(ctx, err)
}

func (a *myInboundAdapter) writePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	defer buffer.Release()
	if destination.IsFqdn() {
		udpAddr, err := net.ResolveUDPAddr(N.NetworkUDP, destination.String())
		if err != nil {
			return err
		}
		return common.Error(a.udpConn.WriteTo(buffer.Bytes(), udpAddr))
	}
	return common.Error(a.udpConn.WriteToUDPAddrPort(buffer.Bytes(), destination.AddrPort()))
}

type myInboundPacketAdapter myInboundAdapter

func (s *myInboundPacketAdapter) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	n, addr, err := s.udpConn.ReadFromUDPAddrPort(buffer.FreeBytes())
	if err != nil {
		return M.Socksaddr{}, err
	}
	buffer.Truncate(n)
	return M.SocksaddrFromNetIP(addr), nil
}

func (s *myInboundPacketAdapter) WriteIsThreadUnsafe() {
}

type myInboundPacket struct {
	buffer      *buf.Buffer
	destination M.Socksaddr
}

func (s *myInboundPacketAdapter) Upstream() any {
	return s.udpConn
}

func (s *myInboundPacketAdapter) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	s.packetAccess.RLock()
	defer s.packetAccess.RUnlock()

	select {
	case <-s.packetOutboundClosed:
		return os.ErrClosed
	default:
	}

	s.packetOutbound <- &myInboundPacket{buffer, destination}
	return nil
}

func (s *myInboundPacketAdapter) Close() error {
	return s.udpConn.Close()
}

func (s *myInboundPacketAdapter) LocalAddr() net.Addr {
	return s.udpConn.LocalAddr()
}

func (s *myInboundPacketAdapter) SetDeadline(t time.Time) error {
	return s.udpConn.SetDeadline(t)
}

func (s *myInboundPacketAdapter) SetReadDeadline(t time.Time) error {
	return s.udpConn.SetReadDeadline(t)
}

func (s *myInboundPacketAdapter) SetWriteDeadline(t time.Time) error {
	return s.udpConn.SetWriteDeadline(t)
}
