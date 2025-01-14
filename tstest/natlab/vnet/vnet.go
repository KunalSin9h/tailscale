// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package vnet simulates a virtual Internet containing a set of networks with various
// NAT behaviors. You can then plug VMs into the virtual internet at different points
// to test Tailscale working end-to-end in various network conditions.
//
// See https://github.com/tailscale/tailscale/issues/13038
package vnet

// TODO:
// - [ ] port mapping actually working
// - [ ] conf to let you firewall things
// - [ ] tests for NAT tables

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gaissmai/bart"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"go4.org/mem"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
	"tailscale.com/client/tailscale"
	"tailscale.com/derp"
	"tailscale.com/derp/derphttp"
	"tailscale.com/net/netutil"
	"tailscale.com/net/stun"
	"tailscale.com/syncs"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/util/mak"
	"tailscale.com/util/must"
	"tailscale.com/util/set"
	"tailscale.com/util/zstdframe"
)

const nicID = 1

const (
	stunPort = 3478
	pcpPort  = 5351
	ssdpPort = 1900
)

func (s *Server) PopulateDERPMapIPs() error {
	out, err := exec.Command("tailscale", "debug", "derp-map").Output()
	if err != nil {
		return fmt.Errorf("tailscale debug derp-map: %v", err)
	}
	var dm tailcfg.DERPMap
	if err := json.Unmarshal(out, &dm); err != nil {
		return fmt.Errorf("unmarshal DERPMap: %v", err)
	}
	for _, r := range dm.Regions {
		for _, n := range r.Nodes {
			if n.IPv4 != "" {
				s.derpIPs.Add(netip.MustParseAddr(n.IPv4))
			}
		}
	}
	return nil
}

func (n *network) InitNAT(natType NAT) error {
	ctor, ok := natTypes[natType]
	if !ok {
		return fmt.Errorf("unknown NAT type %q", natType)
	}
	t, err := ctor(n)
	if err != nil {
		return fmt.Errorf("error creating NAT type %q for network %v: %w", natType, n.wanIP4, err)
	}
	n.setNATTable(t)
	n.natStyle.Store(natType)
	return nil
}

func (n *network) setNATTable(nt NATTable) {
	n.natMu.Lock()
	defer n.natMu.Unlock()
	n.natTable = nt
}

// SoleLANIP implements [IPPool].
func (n *network) SoleLANIP() (netip.Addr, bool) {
	if len(n.nodesByIP) != 1 {
		return netip.Addr{}, false
	}
	for ip := range n.nodesByIP {
		return ip, true
	}
	return netip.Addr{}, false
}

// WANIP implements [IPPool].
func (n *network) WANIP() netip.Addr { return n.wanIP4 }

func (n *network) initStack() error {
	n.ns = stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol,
			ipv6.NewProtocol,
			arp.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
			icmp.NewProtocol4,
		},
	})
	sackEnabledOpt := tcpip.TCPSACKEnabled(true) // TCP SACK is disabled by default
	tcpipErr := n.ns.SetTransportProtocolOption(tcp.ProtocolNumber, &sackEnabledOpt)
	if tcpipErr != nil {
		return fmt.Errorf("SetTransportProtocolOption SACK: %v", tcpipErr)
	}
	n.linkEP = channel.New(512, 1500, tcpip.LinkAddress(n.mac.HWAddr()))
	if tcpipProblem := n.ns.CreateNIC(nicID, n.linkEP); tcpipProblem != nil {
		return fmt.Errorf("CreateNIC: %v", tcpipProblem)
	}
	n.ns.SetPromiscuousMode(nicID, true)
	n.ns.SetSpoofing(nicID, true)

	var routes []tcpip.Route

	if n.v4 {
		prefix := tcpip.AddrFrom4Slice(n.lanIP4.Addr().AsSlice()).WithPrefix()
		prefix.PrefixLen = n.lanIP4.Bits()
		if tcpProb := n.ns.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
			Protocol:          ipv4.ProtocolNumber,
			AddressWithPrefix: prefix,
		}, stack.AddressProperties{}); tcpProb != nil {
			return errors.New(tcpProb.String())
		}

		ipv4Subnet, err := tcpip.NewSubnet(tcpip.AddrFromSlice(make([]byte, 4)), tcpip.MaskFromBytes(make([]byte, 4)))
		if err != nil {
			return fmt.Errorf("could not create IPv4 subnet: %v", err)
		}
		routes = append(routes, tcpip.Route{
			Destination: ipv4Subnet,
			NIC:         nicID,
		})
	}
	if n.v6 {
		prefix := tcpip.AddrFrom16(n.wanIP6.Addr().As16()).WithPrefix()
		prefix.PrefixLen = n.wanIP6.Bits()
		if tcpProb := n.ns.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
			Protocol:          ipv6.ProtocolNumber,
			AddressWithPrefix: prefix,
		}, stack.AddressProperties{}); tcpProb != nil {
			return errors.New(tcpProb.String())
		}

		ipv6Subnet, err := tcpip.NewSubnet(tcpip.AddrFromSlice(make([]byte, 16)), tcpip.MaskFromBytes(make([]byte, 16)))
		if err != nil {
			return fmt.Errorf("could not create IPv6 subnet: %v", err)
		}
		routes = append(routes, tcpip.Route{
			Destination: ipv6Subnet,
			NIC:         nicID,
		})
	}

	n.ns.SetRouteTable(routes)

	const tcpReceiveBufferSize = 0 // default
	const maxInFlightConnectionAttempts = 8192
	tcpFwd := tcp.NewForwarder(n.ns, tcpReceiveBufferSize, maxInFlightConnectionAttempts, n.acceptTCP)
	n.ns.SetTransportProtocolHandler(tcp.ProtocolNumber, func(tei stack.TransportEndpointID, pb *stack.PacketBuffer) (handled bool) {
		return tcpFwd.HandlePacket(tei, pb)
	})

	go func() {
		for {
			pkt := n.linkEP.ReadContext(n.s.shutdownCtx)
			if pkt == nil {
				if n.s.shutdownCtx.Err() != nil {
					// Return without logging.
					return
				}
				continue
			}
			n.handleIPPacketFromGvisor(pkt.ToView().AsSlice())
		}
	}()
	return nil
}

func (n *network) handleIPPacketFromGvisor(ipRaw []byte) {
	if len(ipRaw) == 0 {
		panic("empty packet from gvisor")
	}
	var goPkt gopacket.Packet
	ipVer := ipRaw[0] >> 4 // 4 or 6
	switch ipVer {
	case 4:
		goPkt = gopacket.NewPacket(
			ipRaw,
			layers.LayerTypeIPv4, gopacket.Lazy)
	case 6:
		goPkt = gopacket.NewPacket(
			ipRaw,
			layers.LayerTypeIPv6, gopacket.Lazy)
	default:
		return
	}
	flow, ok := flow(goPkt)
	if !ok {
		panic("unexpected gvisor packet")
	}
	node, ok := n.nodeForDestIP(flow.dst)
	if !ok {
		n.logf("no node for netstack dest IP %v", flow.dst)
		return
	}
	eth := &layers.Ethernet{
		SrcMAC:       n.mac.HWAddr(),
		DstMAC:       node.mac.HWAddr(),
		EthernetType: flow.etherType(),
	}
	buffer := gopacket.NewSerializeBuffer()
	options := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	sls := []gopacket.SerializableLayer{
		eth,
	}
	for _, layer := range goPkt.Layers() {
		sl, ok := layer.(gopacket.SerializableLayer)
		if !ok {
			log.Fatalf("layer %s is not serializable", layer.LayerType().String())
		}
		switch gl := layer.(type) {
		case *layers.TCP:
			gl.SetNetworkLayerForChecksum(goPkt.NetworkLayer())
		case *layers.UDP:
			gl.SetNetworkLayerForChecksum(goPkt.NetworkLayer())
		}
		sls = append(sls, sl)
	}

	if err := gopacket.SerializeLayers(buffer, options, sls...); err != nil {
		n.logf("gvisor: serialize error: %v", err)
		return
	}
	if nw, ok := n.writers.Load(node.mac); ok {
		nw.write(buffer.Bytes())
	} else {
		n.logf("gvisor write: no writeFunc for %v", node.mac)
	}
}

func netaddrIPFromNetstackIP(s tcpip.Address) netip.Addr {
	switch s.Len() {
	case 4:
		return netip.AddrFrom4(s.As4())
	case 16:
		return netip.AddrFrom16(s.As16()).Unmap()
	}
	return netip.Addr{}
}

func stringifyTEI(tei stack.TransportEndpointID) string {
	localHostPort := net.JoinHostPort(tei.LocalAddress.String(), strconv.Itoa(int(tei.LocalPort)))
	remoteHostPort := net.JoinHostPort(tei.RemoteAddress.String(), strconv.Itoa(int(tei.RemotePort)))
	return fmt.Sprintf("%s -> %s", remoteHostPort, localHostPort)
}

func (n *network) acceptTCP(r *tcp.ForwarderRequest) {
	reqDetails := r.ID()

	clientRemoteIP := netaddrIPFromNetstackIP(reqDetails.RemoteAddress)
	destIP := netaddrIPFromNetstackIP(reqDetails.LocalAddress)
	destPort := reqDetails.LocalPort
	if !clientRemoteIP.IsValid() {
		r.Complete(true) // sends a RST
		return
	}

	log.Printf("vnet-AcceptTCP: %v", stringifyTEI(reqDetails))

	var wq waiter.Queue
	ep, err := r.CreateEndpoint(&wq)
	if err != nil {
		log.Printf("CreateEndpoint error for %s: %v", stringifyTEI(reqDetails), err)
		r.Complete(true) // sends a RST
		return
	}
	ep.SocketOptions().SetKeepAlive(true)

	if destPort == 123 {
		r.Complete(false)
		tc := gonet.NewTCPConn(&wq, ep)
		io.WriteString(tc, "Hello from Go\nGoodbye.\n")
		tc.Close()
		return
	}

	if destPort == 8008 && fakeTestAgent.Match(destIP) {
		r.Complete(false)
		tc := gonet.NewTCPConn(&wq, ep)
		node := n.nodesByIP[clientRemoteIP]
		ac := &agentConn{node, tc}
		n.s.addIdleAgentConn(ac)
		return
	}

	if destPort == 80 && fakeControl.Match(destIP) {
		r.Complete(false)
		tc := gonet.NewTCPConn(&wq, ep)
		hs := &http.Server{Handler: n.s.control}
		go hs.Serve(netutil.NewOneConnListener(tc, nil))
		return
	}

	if fakeDERP1.Match(destIP) || fakeDERP2.Match(destIP) {
		if destPort == 443 {
			ds := n.s.derps[0]
			if fakeDERP2.Match(destIP) {
				ds = n.s.derps[1]
			}

			r.Complete(false)
			tc := gonet.NewTCPConn(&wq, ep)
			tlsConn := tls.Server(tc, ds.tlsConfig)
			hs := &http.Server{Handler: ds.handler}
			go hs.Serve(netutil.NewOneConnListener(tlsConn, nil))
			return
		}
		if destPort == 80 {
			r.Complete(false)
			tc := gonet.NewTCPConn(&wq, ep)
			hs := &http.Server{Handler: n.s.derps[0].handler}
			go hs.Serve(netutil.NewOneConnListener(tc, nil))
			return
		}
	}
	if destPort == 443 && fakeLogCatcher.Match(destIP) {
		r.Complete(false)
		tc := gonet.NewTCPConn(&wq, ep)
		go n.serveLogCatcherConn(clientRemoteIP, tc)
		return
	}

	var targetDial string
	if n.s.derpIPs.Contains(destIP) {
		targetDial = destIP.String() + ":" + strconv.Itoa(int(destPort))
	} else if fakeProxyControlplane.Match(destIP) {
		targetDial = "controlplane.tailscale.com:" + strconv.Itoa(int(destPort))
	}
	if targetDial != "" {
		c, err := net.Dial("tcp", targetDial)
		if err != nil {
			r.Complete(true)
			log.Printf("Dial controlplane: %v", err)
			return
		}
		defer c.Close()
		tc := gonet.NewTCPConn(&wq, ep)
		defer tc.Close()
		r.Complete(false)
		errc := make(chan error, 2)
		go func() { _, err := io.Copy(tc, c); errc <- err }()
		go func() { _, err := io.Copy(c, tc); errc <- err }()
		<-errc
	} else {
		r.Complete(true) // sends a RST
	}
}

// serveLogCatchConn serves a TCP connection to "log.tailscale.io", speaking the
// logtail/logcatcher protocol.
//
// We terminate TLS with an arbitrary cert; the client is configured to not
// validate TLS certs for this hostname when running under these integration
// tests.
func (n *network) serveLogCatcherConn(clientRemoteIP netip.Addr, c net.Conn) {
	tlsConfig := n.s.derps[0].tlsConfig // self-signed (stealing DERP's); test client configure to not check
	tlsConn := tls.Server(c, tlsConfig)
	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		all, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Encoding") == "zstd" {
			var err error
			all, err = zstdframe.AppendDecode(nil, all)
			if err != nil {
				log.Printf("LOGS DECODE ERROR zstd decode: %v", err)
				http.Error(w, "zstd decode error", http.StatusBadRequest)
				return
			}
		}
		var logs []struct {
			Logtail struct {
				Client_Time time.Time
			}
			Text string
		}
		if err := json.Unmarshal(all, &logs); err != nil {
			log.Printf("Logs decode error: %v", err)
			return
		}
		node := n.nodesByIP[clientRemoteIP]
		if node != nil {
			node.logMu.Lock()
			defer node.logMu.Unlock()
			node.logCatcherWrites++
			for _, lg := range logs {
				tStr := lg.Logtail.Client_Time.Round(time.Millisecond).Format(time.RFC3339Nano)
				fmt.Fprintf(&node.logBuf, "[%v] %s\n", tStr, lg.Text)
			}
		}
	})
	hs := &http.Server{Handler: handler}
	hs.Serve(netutil.NewOneConnListener(tlsConn, nil))
}

type EthernetPacket struct {
	le *layers.Ethernet
	gp gopacket.Packet
}

func (ep EthernetPacket) SrcMAC() MAC {
	return MAC(ep.le.SrcMAC)
}

func (ep EthernetPacket) DstMAC() MAC {
	return MAC(ep.le.DstMAC)
}

type MAC [6]byte

func (m MAC) IsBroadcast() bool {
	return m == MAC{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
}

// IsIPv6Multicast reports whether m is an IPv6 multicast MAC address,
// typically one containing a solicited-node multicast address.
func (m MAC) IsIPv6Multicast() bool {
	return m[0] == 0x33 && m[1] == 0x33
}

func macOf(hwa net.HardwareAddr) (_ MAC, ok bool) {
	if len(hwa) != 6 {
		return MAC{}, false
	}
	return MAC(hwa), true
}

func (m MAC) HWAddr() net.HardwareAddr {
	return net.HardwareAddr(m[:])
}

func (m MAC) String() string {
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", m[0], m[1], m[2], m[3], m[4], m[5])
}

type portMapping struct {
	dst    netip.AddrPort // LAN IP:port
	expiry time.Time
}

type writerFunc func([]byte, *net.UnixAddr, int)

// Encapsulates both a write function, an optional outbound socket address
// for dgram mode and an interfaceID for packet captures.
type networkWriter struct {
	writer      writerFunc    // Function to write packets to the network
	addr        *net.UnixAddr // Outbound socket address for dgram mode
	interfaceID int           // The interface ID of the src node (for writing pcaps)
}

func (nw *networkWriter) write(b []byte) {
	nw.writer(b, nw.addr, nw.interfaceID)
}

type network struct {
	s              *Server
	num            int // 1-based
	mac            MAC // of router
	portmap        bool
	lanInterfaceID int
	wanInterfaceID int
	v4             bool                 // network supports IPv4
	v6             bool                 // network support IPv6
	wanIP6         netip.Prefix         // router's WAN IPv6, if any, as a /64.
	wanIP4         netip.Addr           // router's LAN IPv4, if any
	lanIP4         netip.Prefix         // router's LAN IP + CIDR (e.g. 192.168.2.1/24)
	nodesByIP      map[netip.Addr]*node // by LAN IPv4
	nodesByMAC     map[MAC]*node
	logf           func(format string, args ...any)

	ns     *stack.Stack
	linkEP *channel.Endpoint

	natStyle    syncs.AtomicValue[NAT]
	natMu       sync.Mutex // held while using + changing natTable
	natTable    NATTable
	portMap     map[netip.AddrPort]portMapping    // WAN ip:port -> LAN ip:port
	portMapFlow map[portmapFlowKey]netip.AddrPort // (lanAP, peerWANAP) -> portmapped wanAP

	macMu     sync.Mutex
	macOfIPv6 map[netip.Addr]MAC // IPv6 source IP -> MAC

	// writers is a map of MAC -> networkWriters to write packets to that MAC.
	// It contains entries for connected nodes only.
	writers syncs.Map[MAC, networkWriter] // MAC -> to networkWriter for that MAC
}

// Regsiters a writerFunc for a MAC address.
// raddr is and optional outbound socket address of the client interface for dgram mode.
// Pass nil for the writerFunc to deregister the writer.
func (n *network) registerWriter(mac MAC, raddr *net.UnixAddr, interfaceID int, wf writerFunc) {
	if wf != nil {
		n.writers.Store(mac, networkWriter{
			writer:      wf,
			addr:        raddr,
			interfaceID: interfaceID,
		})
	} else {
		n.writers.Delete(mac)
	}
}

func (n *network) MACOfIP(ip netip.Addr) (_ MAC, ok bool) {
	if n.lanIP4.Addr() == ip {
		return n.mac, true
	}
	if n, ok := n.nodesByIP[ip]; ok {
		return n.mac, true
	}
	return MAC{}, false
}

type node struct {
	mac           MAC
	num           int // 1-based node number
	interfaceID   int
	net           *network
	lanIP         netip.Addr // must be in net.lanIP prefix + unique in net
	verboseSyslog bool

	// logMu guards logBuf.
	// TODO(bradfitz): conditionally write these out to separate files at the end?
	// Currently they only hold logcatcher logs.
	logMu            sync.Mutex
	logBuf           bytes.Buffer
	logCatcherWrites int
}

// String returns the string "nodeN" where N is the 1-based node number.
func (n *node) String() string {
	return fmt.Sprintf("node%d", n.num)
}

type derpServer struct {
	srv       *derp.Server
	handler   http.Handler
	tlsConfig *tls.Config
}

func newDERPServer() *derpServer {
	// Just to get a self-signed TLS cert:
	ts := httptest.NewTLSServer(nil)
	ts.Close()

	ds := &derpServer{
		srv:       derp.NewServer(key.NewNode(), logger.Discard),
		tlsConfig: ts.TLS, // self-signed; test client configure to not check
	}
	var mux http.ServeMux
	mux.Handle("/derp", derphttp.Handler(ds.srv))
	mux.HandleFunc("/generate_204", derphttp.ServeNoContent)

	ds.handler = &mux
	return ds
}

type Server struct {
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
	shuttingDown   atomic.Bool
	wg             sync.WaitGroup
	blendReality   bool

	derpIPs set.Set[netip.Addr]

	nodes        []*node
	nodeByMAC    map[MAC]*node
	networks     set.Set[*network]
	networkByWAN *bart.Table[*network]

	control    *testcontrol.Server
	derps      []*derpServer
	pcapWriter *pcapWriter

	mu              sync.Mutex
	agentConnWaiter map[*node]chan<- struct{} // signaled after added to set
	agentConns      set.Set[*agentConn]       //  not keyed by node; should be small/cheap enough to scan all
	agentDialer     map[*node]DialFunc
}

type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

var derpMap = &tailcfg.DERPMap{
	Regions: map[int]*tailcfg.DERPRegion{
		1: {
			RegionID:   1,
			RegionCode: "atlantis",
			RegionName: "Atlantis",
			Nodes: []*tailcfg.DERPNode{
				{
					Name:             "1a",
					RegionID:         1,
					HostName:         "derp1.tailscale",
					IPv4:             fakeDERP1.v4.String(),
					IPv6:             fakeDERP1.v6.String(),
					InsecureForTests: true,
					CanPort80:        true,
				},
			},
		},
		2: {
			RegionID:   2,
			RegionCode: "northpole",
			RegionName: "North Pole",
			Nodes: []*tailcfg.DERPNode{
				{
					Name:             "2a",
					RegionID:         2,
					HostName:         "derp2.tailscale",
					IPv4:             fakeDERP2.v4.String(),
					IPv6:             fakeDERP2.v6.String(),
					InsecureForTests: true,
					CanPort80:        true,
				},
			},
		},
	},
}

func New(c *Config) (*Server, error) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		shutdownCtx:    ctx,
		shutdownCancel: cancel,

		control: &testcontrol.Server{
			DERPMap:         derpMap,
			ExplicitBaseURL: "http://control.tailscale",
		},

		blendReality: c.blendReality,
		derpIPs:      set.Of[netip.Addr](),

		nodeByMAC:    map[MAC]*node{},
		networkByWAN: &bart.Table[*network]{},
		networks:     set.Of[*network](),
	}
	for range 2 {
		s.derps = append(s.derps, newDERPServer())
	}
	if err := s.initFromConfig(c); err != nil {
		return nil, err
	}
	for n := range s.networks {
		if err := n.initStack(); err != nil {
			return nil, fmt.Errorf("newServer: initStack: %v", err)
		}
	}

	return s, nil
}

func (s *Server) Close() {
	if shutdown := s.shuttingDown.Swap(true); !shutdown {
		s.shutdownCancel()
		s.pcapWriter.Close()
	}
	s.wg.Wait()
}

func (s *Server) HWAddr(mac MAC) net.HardwareAddr {
	// TODO: cache
	return net.HardwareAddr(mac[:])
}

type Protocol int

const (
	ProtocolQEMU      = Protocol(iota + 1)
	ProtocolUnixDGRAM // for macOS Virtualization.Framework and VZFileHandleNetworkDeviceAttachment
)

// Handles a single connection from a QEMU-style client or muxd connections for dgram mode
func (s *Server) ServeUnixConn(uc *net.UnixConn, proto Protocol) {
	if s.shuttingDown.Load() {
		return
	}
	s.wg.Add(1)
	defer s.wg.Done()
	context.AfterFunc(s.shutdownCtx, func() {
		uc.SetDeadline(time.Now())
	})
	log.Printf("Got conn %T %p", uc, uc)
	defer uc.Close()

	bw := bufio.NewWriterSize(uc, 2<<10)
	var writeMu sync.Mutex
	writePkt := func(pkt []byte, raddr *net.UnixAddr, interfaceID int) {
		if pkt == nil {
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		switch proto {
		case ProtocolQEMU:
			hdr := binary.BigEndian.AppendUint32(bw.AvailableBuffer()[:0], uint32(len(pkt)))
			if _, err := bw.Write(hdr); err != nil {
				log.Printf("Write hdr: %v", err)
				return
			}

			if _, err := bw.Write(pkt); err != nil {
				log.Printf("Write pkt: %v", err)
				return

			}
		case ProtocolUnixDGRAM:
			if raddr == nil {
				log.Printf("Write pkt: dgram mode write failure, no outbound socket address")
				return
			}

			if _, err := uc.WriteToUnix(pkt, raddr); err != nil {
				log.Printf("Write pkt : %v", err)
				return
			}
		}

		if err := bw.Flush(); err != nil {
			log.Printf("Flush: %v", err)
		}
		must.Do(s.pcapWriter.WritePacket(gopacket.CaptureInfo{
			Timestamp:      time.Now(),
			CaptureLength:  len(pkt),
			Length:         len(pkt),
			InterfaceIndex: interfaceID,
		}, pkt))
	}

	buf := make([]byte, 16<<10)
	for {
		var packetRaw []byte
		var raddr *net.UnixAddr

		switch proto {
		case ProtocolUnixDGRAM:
			n, addr, err := uc.ReadFromUnix(buf)
			raddr = addr
			if err != nil {
				log.Printf("ReadFromUnix: %v", err)
				continue
			}
			packetRaw = buf[:n]
		case ProtocolQEMU:
			if _, err := io.ReadFull(uc, buf[:4]); err != nil {
				if s.shutdownCtx.Err() != nil {
					// Return without logging.
					return
				}
				log.Printf("ReadFull header: %v", err)
				return
			}
			n := binary.BigEndian.Uint32(buf[:4])

			if _, err := io.ReadFull(uc, buf[4:4+n]); err != nil {
				if s.shutdownCtx.Err() != nil {
					// Return without logging.
					return
				}
				log.Printf("ReadFull pkt: %v", err)
				return
			}
			packetRaw = buf[4 : 4+n] // raw ethernet frame
		}

		packet := gopacket.NewPacket(packetRaw, layers.LayerTypeEthernet, gopacket.Lazy)
		le, ok := packet.LinkLayer().(*layers.Ethernet)
		if !ok || len(le.SrcMAC) != 6 || len(le.DstMAC) != 6 {
			log.Printf("ignoring non-Ethernet packet: % 02x", packetRaw)
			continue
		}
		ep := EthernetPacket{le, packet}

		srcMAC := ep.SrcMAC()
		srcNode, ok := s.nodeByMAC[srcMAC]
		if !ok {
			log.Printf("[conn %p] got frame from unknown MAC %v", uc, srcMAC)
			continue
		}

		// Register a writer for the source MAC address if one doesn't exist.
		if _, ok := srcNode.net.writers.Load(srcMAC); !ok {
			log.Printf("[conn %p] Registering writer for MAC %v is node %v", uc, srcMAC, srcNode.lanIP)
			srcNode.net.registerWriter(srcMAC, raddr, srcNode.interfaceID, writePkt)
			defer func() {
				srcNode.net.registerWriter(srcMAC, nil, 0, nil)
			}()
			continue
		}

		must.Do(s.pcapWriter.WritePacket(gopacket.CaptureInfo{
			Timestamp:      time.Now(),
			CaptureLength:  len(packetRaw),
			Length:         len(packetRaw),
			InterfaceIndex: srcNode.interfaceID,
		}, packetRaw))
		srcNode.net.HandleEthernetPacket(ep)
	}
}

func (s *Server) routeUDPPacket(up UDPPacket) {
	// Find which network owns this based on the destination IP
	// and all the known networks' wan IPs.

	// But certain things (like STUN) we do in-process.
	if up.Dst.Port() == stunPort {
		// TODO(bradfitz): fake latency; time.AfterFunc the response
		if res, ok := makeSTUNReply(up); ok {
			//log.Printf("STUN reply: %+v", res)
			s.routeUDPPacket(res)
		} else {
			log.Printf("weird: STUN packet not handled")
		}
		return
	}

	dstIP := up.Dst.Addr()
	netw, ok := s.networkByWAN.Lookup(dstIP)
	if !ok {
		if dstIP.IsPrivate() {
			// Not worth spamming logs. RFC 1918 space doesn't route.
			return
		}
		log.Printf("no network to route UDP packet for %v", up.Dst)
		return
	}
	netw.HandleUDPPacket(up)
}

// writeEth writes a raw Ethernet frame to all (0, 1, or multiple) connected
// clients on the network.
//
// This only delivers to client devices and not the virtual router/gateway
// device.
//
// It reports whether a packet was written to any clients.
func (n *network) writeEth(res []byte) bool {
	if len(res) < 12 {
		return false
	}
	dstMAC := MAC(res[0:6])
	srcMAC := MAC(res[6:12])
	if dstMAC.IsBroadcast() {
		num := 0
		n.writers.Range(func(mac MAC, nw networkWriter) bool {
			num++
			nw.write(res)
			return true
		})
		return num > 0
	}
	if srcMAC == dstMAC {
		n.logf("dropping write of packet from %v to itself", srcMAC)
		return false
	}
	if nw, ok := n.writers.Load(dstMAC); ok {
		nw.write(res)
		return true
	}

	const debugMiss = false
	if debugMiss {
		gp := gopacket.NewPacket(res, layers.LayerTypeEthernet, gopacket.Lazy)
		n.logf("no writeFunc for dst %v from src %v; pkt=%v", dstMAC, srcMAC, gp)
	}

	return false
}

var (
	macAllRouters = MAC{0: 0x33, 1: 0x33, 5: 0x02}
)

func (n *network) HandleEthernetPacket(ep EthernetPacket) {
	packet := ep.gp
	dstMAC := ep.DstMAC()
	isBroadcast := dstMAC.IsBroadcast()
	isV6SpecialMAC := dstMAC[0] == 0x33 && dstMAC[1] == 0x33

	// forRouter is whether the packet is destined for the router itself
	// or if it's a special thing (like V6 NDP) that the router should handle.
	forRouter := dstMAC == n.mac || isBroadcast || isV6SpecialMAC

	const debug = false
	if debug {
		n.logf("HandleEthernetPacket: %v => %v; type %v, forRouter=%v", ep.SrcMAC(), ep.DstMAC(), ep.le.EthernetType, forRouter)
	}

	switch ep.le.EthernetType {
	default:
		n.logf("Dropping non-IP packet: %v", ep.le.EthernetType)
		return
	case layers.EthernetTypeARP:
		res, err := n.createARPResponse(packet)
		if err != nil {
			n.logf("createARPResponse: %v", err)
		} else {
			n.writeEth(res)
		}
		return
	case layers.EthernetTypeIPv6:
		if !n.v6 {
			n.logf("dropping IPv6 packet on v4-only network")
			return
		}
		if dstMAC == macAllRouters {
			if rs, ok := ep.gp.Layer(layers.LayerTypeICMPv6RouterSolicitation).(*layers.ICMPv6RouterSolicitation); ok {
				n.handleIPv6RouterSolicitation(ep, rs)
			} else {
				n.logf("unexpected IPv6 packet to all-routers: %v", ep.gp)
			}
			return
		}
		isMcast := dstMAC.IsIPv6Multicast()
		if isMcast || dstMAC == n.mac {
			if ns, ok := ep.gp.Layer(layers.LayerTypeICMPv6NeighborSolicitation).(*layers.ICMPv6NeighborSolicitation); ok {
				n.handleIPv6NeighborSolicitation(ep, ns)
				return
			}
			if ep.gp.Layer(layers.LayerTypeMLDv2MulticastListenerReport) != nil {
				// We don't care about these (yet?) and Linux spams a bunch
				// a bunch of them out, so explicitly ignore them to prevent
				// log spam when verbose logging is enabled.
				return
			}
			if isMcast {
				return
			}
		}

		// TODO(bradfitz): handle packets to e.g. [fe80::50cc:ccff:fecc:cc01]:43619
		// and don't fall through to the router below.

	case layers.EthernetTypeIPv4:
		// Below
	}

	// Send ethernet broadcasts and unicast ethernet frames to peers
	// on the same network. This is all LAN traffic that isn't meant
	// for the router/gw itself:
	if isBroadcast || !forRouter {
		n.writeEth(ep.gp.Data())
	}

	if forRouter {
		n.HandleEthernetPacketForRouter(ep)
	}
}

// HandleUDPPacket handles a UDP packet arriving from the internet,
// addressed to the router's WAN IP. It is then NATed back to a
// LAN IP here and wrapped in an ethernet layer and delivered
// to the network.
func (n *network) HandleUDPPacket(p UDPPacket) {
	buf, err := n.serializedUDPPacket(p.Src, p.Dst, p.Payload, nil)
	if err != nil {
		n.logf("serializing UDP packet: %v", err)
		return
	}
	n.s.pcapWriter.WritePacket(gopacket.CaptureInfo{
		Timestamp:      time.Now(),
		CaptureLength:  len(buf),
		Length:         len(buf),
		InterfaceIndex: n.wanInterfaceID,
	}, buf)
	dst := n.doNATIn(p.Src, p.Dst)
	if !dst.IsValid() {
		n.logf("Warning: NAT dropped packet; no mapping for %v=>%v", p.Src, p.Dst)
		return
	}
	p.Dst = dst
	buf, err = n.serializedUDPPacket(p.Src, p.Dst, p.Payload, nil)
	if err != nil {
		n.logf("serializing UDP packet: %v", err)
		return
	}
	n.s.pcapWriter.WritePacket(gopacket.CaptureInfo{
		Timestamp:      time.Now(),
		CaptureLength:  len(buf),
		Length:         len(buf),
		InterfaceIndex: n.lanInterfaceID,
	}, buf)
	n.WriteUDPPacketNoNAT(p)
}

func (n *network) nodeForDestIP(ip netip.Addr) (node *node, ok bool) {
	node, ok = n.nodesByIP[ip]
	if !ok && ip.Is6() {
		var mac MAC
		n.macMu.Lock()
		mac, ok = n.macOfIPv6[ip]
		n.macMu.Unlock()
		if !ok {
			log.Printf("XXX no MAC for IPv6 %v", ip)
			return nil, false
		}
		node, ok = n.nodesByMAC[mac]
		if !ok {
			log.Printf("XXX no node for MAC %v", mac)
		}
	}
	return node, ok
}

// WriteUDPPacketNoNAT writes a UDP packet to the network, without
// doing any NAT translation.
//
// The packet will always have the ethernet src MAC of the router
// so this should not be used for packets between clients on the
// same ethernet segment.
func (n *network) WriteUDPPacketNoNAT(p UDPPacket) {
	src, dst := p.Src, p.Dst
	node, ok := n.nodeForDestIP(dst.Addr())
	if !ok {
		n.logf("no node for dest IP %v in UDP packet %v=>%v", dst.Addr(), p.Src, p.Dst)
		return
	}

	eth := &layers.Ethernet{
		SrcMAC:       n.mac.HWAddr(), // of gateway
		DstMAC:       node.mac.HWAddr(),
		EthernetType: p.etherType(),
	}
	ethRaw, err := n.serializedUDPPacket(src, dst, p.Payload, eth)
	if err != nil {
		n.logf("serializing UDP packet: %v", err)
		return
	}
	n.writeEth(ethRaw)
}

type serializableNetworkLayer interface {
	gopacket.SerializableLayer
	gopacket.NetworkLayer
}

func mkIPLayer(proto layers.IPProtocol, src, dst netip.Addr) serializableNetworkLayer {
	if src.Is4() {
		return &layers.IPv4{
			Version:  4,
			TTL:      64,
			Protocol: proto,
			SrcIP:    src.AsSlice(),
			DstIP:    dst.AsSlice(),
		}
	}
	if src.Is6() {
		return &layers.IPv6{
			Version:    6,
			HopLimit:   64,
			NextHeader: proto,
			SrcIP:      src.AsSlice(),
			DstIP:      dst.AsSlice(),
		}
	}
	panic("invalid src IP")
}

// serializedUDPPacket serializes a UDP packet with the given source and
// destination IP:port pairs, and payload.
//
// If eth is non-nil, it will be used as the Ethernet layer, otherwise the
// Ethernet layer will be omitted from the serialization.
func (n *network) serializedUDPPacket(src, dst netip.AddrPort, payload []byte, eth *layers.Ethernet) ([]byte, error) {
	ip := mkIPLayer(layers.IPProtocolUDP, src.Addr(), dst.Addr())
	udp := &layers.UDP{
		SrcPort: layers.UDPPort(src.Port()),
		DstPort: layers.UDPPort(dst.Port()),
	}
	udp.SetNetworkLayerForChecksum(ip)

	buffer := gopacket.NewSerializeBuffer()
	options := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	layers := []gopacket.SerializableLayer{eth, ip, udp, gopacket.Payload(payload)}
	if eth == nil {
		layers = layers[1:]
	}
	if err := gopacket.SerializeLayers(buffer, options, layers...); err != nil {
		return nil, fmt.Errorf("serializing UDP from %v to %v: %v", src, dst, err)
	}
	return buffer.Bytes(), nil
}

// HandleEthernetPacketForRouter handles an IPv4 packet that is
// directed to the router/gateway itself. The packet may be to the
// broadcast MAC address, or to the router's MAC address. The target
// IP may be the router's IP, or an internet (routed) IP.
func (n *network) HandleEthernetPacketForRouter(ep EthernetPacket) {
	packet := ep.gp
	flow, ok := flow(packet)
	if !ok {
		n.logf("dropping non-IP packet: %v", packet)
		return
	}
	srcIP, dstIP := flow.src, flow.dst

	toForward := dstIP != n.lanIP4.Addr() && dstIP != netip.IPv4Unspecified() && !dstIP.IsLinkLocalUnicast()
	udp, isUDP := packet.Layer(layers.LayerTypeUDP).(*layers.UDP)

	// Pre-NAT mapping, for DNS/etc responses:
	if srcIP.Is6() {
		n.macMu.Lock()
		mak.Set(&n.macOfIPv6, srcIP, ep.SrcMAC())
		n.macMu.Unlock()
	}

	if isDHCPRequest(packet) {
		res, err := n.s.createDHCPResponse(packet)
		if err != nil {
			n.logf("createDHCPResponse: %v", err)
			return
		}
		n.writeEth(res)
		return
	}

	if isMDNSQuery(packet) || isIGMP(packet) {
		// Don't log. Spammy for now.
		return
	}

	if isDNSRequest(packet) {
		res, err := n.s.createDNSResponse(packet)
		if err != nil {
			n.logf("createDNSResponse: %v", err)
			return
		}
		n.writeEth(res)
		return
	}

	if isUDP && fakeSyslog.Match(dstIP) {
		node, ok := n.nodeForDestIP(srcIP)
		if !ok {
			return
		}
		if node.verboseSyslog {
			// TODO(bradfitz): parse this and capture it, structured, into
			// node's log buffer.
			log.Printf("syslog from %v: %s", node, udp.Payload)
		}
		return
	}

	if !toForward && isNATPMP(packet) {
		n.handleNATPMPRequest(UDPPacket{
			Src:     netip.AddrPortFrom(srcIP, uint16(udp.SrcPort)),
			Dst:     netip.AddrPortFrom(dstIP, uint16(udp.DstPort)),
			Payload: udp.Payload,
		})
		return
	}

	if toForward && isUDP {
		src := netip.AddrPortFrom(srcIP, uint16(udp.SrcPort))
		dst := netip.AddrPortFrom(dstIP, uint16(udp.DstPort))
		buf, err := n.serializedUDPPacket(src, dst, udp.Payload, nil)
		if err != nil {
			n.logf("serializing UDP packet: %v", err)
			return
		}
		n.s.pcapWriter.WritePacket(gopacket.CaptureInfo{
			Timestamp:      time.Now(),
			CaptureLength:  len(buf),
			Length:         len(buf),
			InterfaceIndex: n.lanInterfaceID,
		}, buf)

		lanSrc := src // the original src, before NAT (for logging only)
		src = n.doNATOut(src, dst)
		if !src.IsValid() {
			n.logf("warning: NAT dropped packet; no NAT out mapping for %v=>%v", lanSrc, dst)
			return
		}
		buf, err = n.serializedUDPPacket(src, dst, udp.Payload, nil)
		if err != nil {
			n.logf("serializing UDP packet: %v", err)
			return
		}
		n.s.pcapWriter.WritePacket(gopacket.CaptureInfo{
			Timestamp:      time.Now(),
			CaptureLength:  len(buf),
			Length:         len(buf),
			InterfaceIndex: n.wanInterfaceID,
		}, buf)

		if src.Addr().Is6() {
			n.macMu.Lock()
			mak.Set(&n.macOfIPv6, src.Addr(), ep.SrcMAC())
			n.macMu.Unlock()
		}

		n.s.routeUDPPacket(UDPPacket{
			Src:     src,
			Dst:     dst,
			Payload: udp.Payload,
		})
		return
	}

	if toForward && n.s.shouldInterceptTCP(packet) {
		var base *layers.BaseLayer
		proto := header.IPv4ProtocolNumber
		if v4, ok := packet.Layer(layers.LayerTypeIPv4).(*layers.IPv4); ok {
			base = &v4.BaseLayer
		} else if v6, ok := packet.Layer(layers.LayerTypeIPv6).(*layers.IPv6); ok {
			base = &v6.BaseLayer
			proto = header.IPv6ProtocolNumber
		} else {
			panic("not v4, not v6")
		}
		pktCopy := make([]byte, 0, len(base.Contents)+len(base.Payload))
		pktCopy = append(pktCopy, base.Contents...)
		pktCopy = append(pktCopy, base.Payload...)
		packetBuf := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(pktCopy),
		})
		n.linkEP.InjectInbound(proto, packetBuf)
		packetBuf.DecRef()
		return
	}

	if isUDP && (udp.DstPort == pcpPort || udp.DstPort == ssdpPort) {
		// We handle NAT-PMP, but not these yet.
		// TODO(bradfitz): handle? marginal utility so far.
		// Don't log about them being unknown.
		return
	}

	n.logf("router got unknown packet: %v", packet)
}

func (n *network) handleIPv6RouterSolicitation(ep EthernetPacket, rs *layers.ICMPv6RouterSolicitation) {
	v6 := ep.gp.Layer(layers.LayerTypeIPv6).(*layers.IPv6)

	// Send a router advertisement back.
	eth := &layers.Ethernet{
		SrcMAC:       n.mac.HWAddr(),
		DstMAC:       ep.SrcMAC().HWAddr(),
		EthernetType: layers.EthernetTypeIPv6,
	}
	n.logf("sending IPv6 router advertisement to %v from %v", eth.SrcMAC, eth.DstMAC)
	ip := &layers.IPv6{
		Version:    6,
		HopLimit:   255,
		NextHeader: layers.IPProtocolICMPv6,
		SrcIP:      net.ParseIP("fe80::1"),
		DstIP:      v6.SrcIP,
	}
	icmp := &layers.ICMPv6{
		TypeCode: layers.CreateICMPv6TypeCode(layers.ICMPv6TypeRouterAdvertisement, 0),
	}
	pfx := make([]byte, 0, 30)                      // it's 32 on the wire, once gopacket adds two byte header
	pfx = append(pfx, byte(64))                     // CIDR length
	pfx = append(pfx, byte(0xc0))                   // flags: On-Link, Autonomous
	pfx = binary.BigEndian.AppendUint32(pfx, 86400) // valid lifetime
	pfx = binary.BigEndian.AppendUint32(pfx, 14400) // preferred lifetime
	pfx = binary.BigEndian.AppendUint32(pfx, 0)     // reserved
	wanIP := n.wanIP6.Addr().As16()
	pfx = append(pfx, wanIP[:]...)

	ra := &layers.ICMPv6RouterAdvertisement{
		RouterLifetime: 1800,
		Options: []layers.ICMPv6Option{
			{
				Type: layers.ICMPv6OptPrefixInfo,
				Data: pfx,
			},
		},
	}
	icmp.SetNetworkLayerForChecksum(ip)
	buffer := gopacket.NewSerializeBuffer()
	options := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	if err := gopacket.SerializeLayers(buffer, options, eth, ip, icmp, ra); err != nil {
		n.logf("serializing ICMPv6 RA: %v", err)
		return
	}
	n.writeEth(buffer.Bytes())
}

func (n *network) handleIPv6NeighborSolicitation(ep EthernetPacket, ns *layers.ICMPv6NeighborSolicitation) {
	v6 := ep.gp.Layer(layers.LayerTypeIPv6).(*layers.IPv6)

	targetIP, ok := netip.AddrFromSlice(ns.TargetAddress)
	if !ok {
		return
	}
	var srcMAC MAC
	if targetIP == netip.MustParseAddr("fe80::1") {
		srcMAC = n.mac
	} else {
		n.logf("Ignoring IPv6 NS request from %v for target %v", ep.SrcMAC(), targetIP)
		return
	}
	n.logf("replying to IPv6 NS %v->%v about target %v (replySrc=%v)", ep.SrcMAC(), ep.DstMAC(), targetIP, srcMAC)

	// Send a neighbor advertisement back.
	eth := &layers.Ethernet{
		SrcMAC:       srcMAC.HWAddr(),
		DstMAC:       ep.SrcMAC().HWAddr(),
		EthernetType: layers.EthernetTypeIPv6,
	}
	ip := &layers.IPv6{
		Version:    6,
		HopLimit:   255,
		NextHeader: layers.IPProtocolICMPv6,
		SrcIP:      ns.TargetAddress,
		DstIP:      v6.SrcIP,
	}
	icmp := &layers.ICMPv6{
		TypeCode: layers.CreateICMPv6TypeCode(layers.ICMPv6TypeNeighborAdvertisement, 0),
	}
	var flags uint8 = 0x40 // solicited
	if srcMAC == n.mac {
		flags |= 0x80 // router
	}
	flags |= 0x20 // override

	na := &layers.ICMPv6NeighborAdvertisement{
		TargetAddress: ns.TargetAddress,
		Flags:         flags,
	}
	na.Options = append(na.Options, layers.ICMPv6Option{
		Type: layers.ICMPv6OptTargetAddress,
		Data: srcMAC.HWAddr(),
	})
	icmp.SetNetworkLayerForChecksum(ip)
	buffer := gopacket.NewSerializeBuffer()
	options := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	if err := gopacket.SerializeLayers(buffer, options, eth, ip, icmp, na); err != nil {
		n.logf("serializing ICMPv6 RA: %v", err)
		return
	}
	if !n.writeEth(buffer.Bytes()) {
		n.logf("failed to writeEth for IPv6 NA reply for %v", targetIP)
	}
}

// createDHCPResponse creates a DHCPv4 response for the given DHCPv4 request.
func (s *Server) createDHCPResponse(request gopacket.Packet) ([]byte, error) {
	ethLayer := request.Layer(layers.LayerTypeEthernet).(*layers.Ethernet)
	srcMAC, ok := macOf(ethLayer.SrcMAC)
	if !ok {
		return nil, nil
	}
	node, ok := s.nodeByMAC[srcMAC]
	if !ok {
		log.Printf("DHCP request from unknown node %v; ignoring", srcMAC)
		return nil, nil
	}
	gwIP := node.net.lanIP4.Addr()

	ipLayer := request.Layer(layers.LayerTypeIPv4).(*layers.IPv4)
	udpLayer := request.Layer(layers.LayerTypeUDP).(*layers.UDP)
	dhcpLayer := request.Layer(layers.LayerTypeDHCPv4).(*layers.DHCPv4)

	response := &layers.DHCPv4{
		Operation:    layers.DHCPOpReply,
		HardwareType: layers.LinkTypeEthernet,
		HardwareLen:  6,
		Xid:          dhcpLayer.Xid,
		ClientHWAddr: dhcpLayer.ClientHWAddr,
		Flags:        dhcpLayer.Flags,
		YourClientIP: node.lanIP.AsSlice(),
		Options: []layers.DHCPOption{
			{
				Type:   layers.DHCPOptServerID,
				Data:   gwIP.AsSlice(), // DHCP server's IP
				Length: 4,
			},
		},
	}

	var msgType layers.DHCPMsgType
	for _, opt := range dhcpLayer.Options {
		if opt.Type == layers.DHCPOptMessageType && opt.Length > 0 {
			msgType = layers.DHCPMsgType(opt.Data[0])
		}
	}
	switch msgType {
	case layers.DHCPMsgTypeDiscover:
		response.Options = append(response.Options, layers.DHCPOption{
			Type:   layers.DHCPOptMessageType,
			Data:   []byte{byte(layers.DHCPMsgTypeOffer)},
			Length: 1,
		})
	case layers.DHCPMsgTypeRequest:
		response.Options = append(response.Options,
			layers.DHCPOption{
				Type:   layers.DHCPOptMessageType,
				Data:   []byte{byte(layers.DHCPMsgTypeAck)},
				Length: 1,
			},
			layers.DHCPOption{
				Type:   layers.DHCPOptLeaseTime,
				Data:   binary.BigEndian.AppendUint32(nil, 3600), // hour? sure.
				Length: 4,
			},
			layers.DHCPOption{
				Type:   layers.DHCPOptRouter,
				Data:   gwIP.AsSlice(),
				Length: 4,
			},
			layers.DHCPOption{
				Type:   layers.DHCPOptDNS,
				Data:   fakeDNS.v4.AsSlice(),
				Length: 4,
			},
			layers.DHCPOption{
				Type:   layers.DHCPOptSubnetMask,
				Data:   net.CIDRMask(node.net.lanIP4.Bits(), 32),
				Length: 4,
			},
		)
	}

	eth := &layers.Ethernet{
		SrcMAC:       node.net.mac.HWAddr(),
		DstMAC:       ethLayer.SrcMAC,
		EthernetType: layers.EthernetTypeIPv4, // never IPv6 for DHCP
	}

	ip := &layers.IPv4{
		Version:  4,
		TTL:      64,
		Protocol: layers.IPProtocolUDP,
		SrcIP:    ipLayer.DstIP,
		DstIP:    ipLayer.SrcIP,
	}

	udp := &layers.UDP{
		SrcPort: udpLayer.DstPort,
		DstPort: udpLayer.SrcPort,
	}
	udp.SetNetworkLayerForChecksum(ip)

	buffer := gopacket.NewSerializeBuffer()
	options := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	if err := gopacket.SerializeLayers(buffer, options,
		eth,
		ip,
		udp,
		response,
	); err != nil {
		return nil, err
	}

	return buffer.Bytes(), nil
}

func isDHCPRequest(pkt gopacket.Packet) bool {
	v4, ok := pkt.Layer(layers.LayerTypeIPv4).(*layers.IPv4)
	if !ok || v4.Protocol != layers.IPProtocolUDP {
		return false
	}
	udp, ok := pkt.Layer(layers.LayerTypeUDP).(*layers.UDP)
	return ok && udp.DstPort == 67 && udp.SrcPort == 68
}

func isIGMP(pkt gopacket.Packet) bool {
	return pkt.Layer(layers.LayerTypeIGMP) != nil
}

func isMDNSQuery(pkt gopacket.Packet) bool {
	udp, ok := pkt.Layer(layers.LayerTypeUDP).(*layers.UDP)
	// TODO(bradfitz): also check IPv4 DstIP=224.0.0.251 (or whatever)
	return ok && udp.SrcPort == 5353 && udp.DstPort == 5353
}

func (s *Server) shouldInterceptTCP(pkt gopacket.Packet) bool {
	tcp, ok := pkt.Layer(layers.LayerTypeTCP).(*layers.TCP)
	if !ok {
		return false
	}
	if tcp.DstPort == 123 {
		// Test port for TCP interception. Not really useful, but cute for
		// demos.
		return true
	}
	flow, ok := flow(pkt)
	if !ok {
		return false
	}
	if tcp.DstPort == 80 || tcp.DstPort == 443 {
		for _, v := range []virtualIP{fakeControl, fakeDERP1, fakeDERP2, fakeLogCatcher} {
			if v.Match(flow.dst) {
				return true
			}
		}
		if fakeProxyControlplane.Match(flow.dst) {
			return s.blendReality
		}
		if s.derpIPs.Contains(flow.dst) {
			return true
		}
	}
	if tcp.DstPort == 8008 && fakeTestAgent.Match(flow.dst) {
		// Connection from cmd/tta.
		return true
	}
	return false
}

type ipSrcDst struct {
	src netip.Addr
	dst netip.Addr
}

func (f ipSrcDst) etherType() layers.EthernetType {
	if f.dst.Is6() {
		return layers.EthernetTypeIPv6
	}
	return layers.EthernetTypeIPv4
}

func (p UDPPacket) etherType() layers.EthernetType {
	if p.Dst.Addr().Is6() {
		return layers.EthernetTypeIPv6
	}
	return layers.EthernetTypeIPv4
}

func flow(gp gopacket.Packet) (f ipSrcDst, ok bool) {
	if gp == nil {
		return f, false
	}
	n := gp.NetworkLayer()
	if n == nil {
		return f, false
	}
	sb, db := n.NetworkFlow().Endpoints()
	src, _ := netip.AddrFromSlice(sb.Raw())
	dst, _ := netip.AddrFromSlice(db.Raw())
	return ipSrcDst{src: src, dst: dst}, src.IsValid() && dst.IsValid()
}

// isDNSRequest reports whether pkt is a DNS request to the fake DNS server.
func isDNSRequest(pkt gopacket.Packet) bool {
	udp, ok := pkt.Layer(layers.LayerTypeUDP).(*layers.UDP)
	if !ok || udp.DstPort != 53 {
		return false
	}
	f, ok := flow(pkt)
	if !ok {
		return false
	}
	if !fakeDNS.Match(f.dst) {
		// TODO(bradfitz): maybe support configs where DNS is local in the LAN
		return false
	}
	dns, ok := pkt.Layer(layers.LayerTypeDNS).(*layers.DNS)
	return ok && dns.QR == false && len(dns.Questions) > 0
}

func isNATPMP(pkt gopacket.Packet) bool {
	udp, ok := pkt.Layer(layers.LayerTypeUDP).(*layers.UDP)
	return ok && udp.DstPort == 5351 && len(udp.Payload) > 0 && udp.Payload[0] == 0 // version 0, not 2 for PCP
}

func makeSTUNReply(req UDPPacket) (res UDPPacket, ok bool) {
	txid, err := stun.ParseBindingRequest(req.Payload)
	if err != nil {
		log.Printf("invalid STUN request: %v", err)
		return res, false
	}
	return UDPPacket{
		Src:     req.Dst,
		Dst:     req.Src,
		Payload: stun.Response(txid, req.Src),
	}, true
}

func (s *Server) createDNSResponse(pkt gopacket.Packet) ([]byte, error) {
	flow, ok := flow(pkt)
	if !ok {
		return nil, nil
	}
	ethLayer := pkt.Layer(layers.LayerTypeEthernet).(*layers.Ethernet)
	udpLayer := pkt.Layer(layers.LayerTypeUDP).(*layers.UDP)
	dnsLayer := pkt.Layer(layers.LayerTypeDNS).(*layers.DNS)

	if dnsLayer.OpCode != layers.DNSOpCodeQuery || dnsLayer.QR || len(dnsLayer.Questions) == 0 {
		return nil, nil
	}

	response := &layers.DNS{
		ID:           dnsLayer.ID,
		QR:           true,
		AA:           true,
		TC:           false,
		RD:           dnsLayer.RD,
		RA:           true,
		OpCode:       layers.DNSOpCodeQuery,
		ResponseCode: layers.DNSResponseCodeNoErr,
	}

	var names []string
	for _, q := range dnsLayer.Questions {
		response.QDCount++
		response.Questions = append(response.Questions, q)

		if mem.HasSuffix(mem.B(q.Name), mem.S(".pool.ntp.org")) {
			// Just drop DNS queries for NTP servers. For Debian/etc guests used
			// during development. Not needed. Assume VM guests get correct time
			// via their hypervisor.
			return nil, nil
		}

		names = append(names, q.Type.String()+"/"+string(q.Name))
		if q.Class != layers.DNSClassIN {
			continue
		}

		if q.Type == layers.DNSTypeA || q.Type == layers.DNSTypeAAAA {
			if v, ok := vips[string(q.Name)]; ok {
				ip := v.v4
				if q.Type == layers.DNSTypeAAAA {
					ip = v.v6
				}
				response.ANCount++
				response.Answers = append(response.Answers, layers.DNSResourceRecord{
					Name:  q.Name,
					Type:  q.Type,
					Class: q.Class,
					IP:    ip.AsSlice(),
					TTL:   60,
				})
			}
		}
	}

	// Make reply layers, all reversed.
	eth2 := &layers.Ethernet{
		SrcMAC:       ethLayer.DstMAC,
		DstMAC:       ethLayer.SrcMAC,
		EthernetType: flow.etherType(),
	}
	ip2 := mkIPLayer(layers.IPProtocolUDP, flow.dst, flow.src)
	udp2 := &layers.UDP{
		SrcPort: udpLayer.DstPort,
		DstPort: udpLayer.SrcPort,
	}
	udp2.SetNetworkLayerForChecksum(ip2)

	buffer := gopacket.NewSerializeBuffer()
	options := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	if err := gopacket.SerializeLayers(buffer, options, eth2, ip2, udp2, response); err != nil {
		return nil, err
	}

	const debugDNS = false
	if debugDNS {
		if len(response.Answers) > 0 {
			back := gopacket.NewPacket(buffer.Bytes(), layers.LayerTypeEthernet, gopacket.Lazy)
			log.Printf("createDNSResponse generated answers: %v", back)
		} else {
			log.Printf("made empty response for %q", names)
		}
	}

	return buffer.Bytes(), nil
}

// doNATOut performs NAT on an outgoing packet from src to dst, where
// src is a LAN IP and dst is a WAN IP.
//
// It returns the source WAN ip:port to use.
//
// If newSrc is invalid, the packet should be dropped.
func (n *network) doNATOut(src, dst netip.AddrPort) (newSrc netip.AddrPort) {
	if src.Addr().Is6() {
		// TODO(bradfitz): IPv6 NAT? For now, normal IPv6 only.
		return src
	}

	n.natMu.Lock()
	defer n.natMu.Unlock()

	// First see if there's a port mapping, before doing NAT.
	if wanAP, ok := n.portMapFlow[portmapFlowKey{
		peerWAN: dst,
		lanAP:   src,
	}]; ok {
		return wanAP
	}

	return n.natTable.PickOutgoingSrc(src, dst, time.Now())
}

type portmapFlowKey struct {
	peerWAN netip.AddrPort // the peer's WAN ip:port
	lanAP   netip.AddrPort
}

// doNATIn performs NAT on an incoming packet from WAN src to WAN dst, returning
// a new destination LAN ip:port to use.
//
// If newDst is invalid, the packet should be dropped.
func (n *network) doNATIn(src, dst netip.AddrPort) (newDst netip.AddrPort) {
	if dst.Addr().Is6() {
		// TODO(bradfitz): IPv6 NAT? For now, normal IPv6 only.
		return dst
	}

	n.natMu.Lock()
	defer n.natMu.Unlock()

	now := time.Now()

	// First see if there's a port mapping, before doing NAT.
	if lanAP, ok := n.portMap[dst]; ok {
		if now.Before(lanAP.expiry) {
			mak.Set(&n.portMapFlow, portmapFlowKey{
				peerWAN: src,
				lanAP:   lanAP.dst,
			}, dst)
			//n.logf("NAT: doNatIn: port mapping %v=>%v", dst, lanAP.dst)
			return lanAP.dst
		}
		n.logf("NAT: doNatIn: port mapping EXPIRED for %v=>%v", dst, lanAP.dst)
		delete(n.portMap, dst)
		return netip.AddrPort{}
	}

	return n.natTable.PickIncomingDst(src, dst, now)
}

// IsPublicPortUsed reports whether the given public port is currently in use.
//
// n.natMu must be held by the caller. (It's only called by nat implementations
// which are always called with natMu held))
func (n *network) IsPublicPortUsed(ap netip.AddrPort) bool {
	_, ok := n.portMap[ap]
	return ok
}

func (n *network) doPortMap(src netip.Addr, dstLANPort, wantExtPort uint16, sec int) (gotPort uint16, ok bool) {
	n.natMu.Lock()
	defer n.natMu.Unlock()

	if !n.portmap {
		return 0, false
	}

	wanAP := netip.AddrPortFrom(n.wanIP4, wantExtPort)
	dst := netip.AddrPortFrom(src, dstLANPort)

	if sec == 0 {
		lanAP, ok := n.portMap[wanAP]
		if ok && lanAP.dst.Addr() == src {
			delete(n.portMap, wanAP)
		}
		return 0, false
	}

	// See if they already have a mapping and extend expiry if so.
	for k, v := range n.portMap {
		if v.dst == dst {
			n.portMap[k] = portMapping{
				dst:    dst,
				expiry: time.Now().Add(time.Duration(sec) * time.Second),
			}
			return k.Port(), true
		}
	}

	for try := 0; try < 20_000; try++ {
		if wanAP.Port() > 0 && !n.natTable.IsPublicPortUsed(wanAP) {
			mak.Set(&n.portMap, wanAP, portMapping{
				dst:    dst,
				expiry: time.Now().Add(time.Duration(sec) * time.Second),
			})
			n.logf("vnet: allocated NAT mapping from %v to %v", wanAP, dst)
			return wanAP.Port(), true
		}
		wantExtPort = rand.N(uint16(32<<10)) + 32<<10
		wanAP = netip.AddrPortFrom(n.wanIP4, wantExtPort)
	}
	return 0, false
}

func (n *network) createARPResponse(pkt gopacket.Packet) ([]byte, error) {
	ethLayer, ok := pkt.Layer(layers.LayerTypeEthernet).(*layers.Ethernet)
	if !ok {
		return nil, nil
	}
	arpLayer, ok := pkt.Layer(layers.LayerTypeARP).(*layers.ARP)
	if !ok ||
		arpLayer.Operation != layers.ARPRequest ||
		arpLayer.AddrType != layers.LinkTypeEthernet ||
		arpLayer.Protocol != layers.EthernetTypeIPv4 ||
		arpLayer.HwAddressSize != 6 ||
		arpLayer.ProtAddressSize != 4 ||
		len(arpLayer.DstProtAddress) != 4 {
		return nil, nil
	}

	wantIP := netip.AddrFrom4([4]byte(arpLayer.DstProtAddress))
	foundMAC, ok := n.MACOfIP(wantIP)
	if !ok {
		return nil, nil
	}

	eth := &layers.Ethernet{
		SrcMAC:       foundMAC.HWAddr(),
		DstMAC:       ethLayer.SrcMAC,
		EthernetType: layers.EthernetTypeARP,
	}

	a2 := &layers.ARP{
		AddrType:          layers.LinkTypeEthernet,
		Protocol:          layers.EthernetTypeIPv4, // never IPv6; IPv6 equivalent of ARP is handleIPv6NeighborSolicitation
		HwAddressSize:     6,
		ProtAddressSize:   4,
		Operation:         layers.ARPReply,
		SourceHwAddress:   foundMAC.HWAddr(),
		SourceProtAddress: arpLayer.DstProtAddress,
		DstHwAddress:      ethLayer.SrcMAC,
		DstProtAddress:    arpLayer.SourceProtAddress,
	}

	buffer := gopacket.NewSerializeBuffer()
	options := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	if err := gopacket.SerializeLayers(buffer, options, eth, a2); err != nil {
		return nil, err
	}

	return buffer.Bytes(), nil
}

func (n *network) handleNATPMPRequest(req UDPPacket) {
	if !n.portmap {
		return
	}
	if string(req.Payload) == "\x00\x00" {
		// https://www.rfc-editor.org/rfc/rfc6886#section-3.2

		res := make([]byte, 0, 12)
		res = append(res,
			0,    // version 0 (NAT-PMP)
			128,  // response to op 0 (128+0)
			0, 0, // result code success
		)
		res = binary.BigEndian.AppendUint32(res, uint32(time.Now().Unix()))
		wan4 := n.wanIP4.As4()
		res = append(res, wan4[:]...)
		n.WriteUDPPacketNoNAT(UDPPacket{
			Src:     req.Dst,
			Dst:     req.Src,
			Payload: res,
		})
		return
	}

	// Map UDP request
	if len(req.Payload) == 12 && req.Payload[0] == 0 && req.Payload[1] == 1 {
		// https://www.rfc-editor.org/rfc/rfc6886#section-3.3
		// "00 01 00 00 ed 40 00 00 00 00 1c 20" =>
		//   00 ver
		//   01 op=map UDP
		//   00 00 reserved  (0 in request; in response, this is the result code)
		//   ed 40 internal port 60736
		//   00 00 suggested external port
		//   00 00 1c 20 suggested lifetime in seconds (7200 sec = 2 hours)
		internalPort := binary.BigEndian.Uint16(req.Payload[4:6])
		wantExtPort := binary.BigEndian.Uint16(req.Payload[6:8])
		lifetimeSec := binary.BigEndian.Uint32(req.Payload[8:12])
		gotPort, ok := n.doPortMap(req.Src.Addr(), internalPort, wantExtPort, int(lifetimeSec))
		if !ok {
			n.logf("NAT-PMP map request for %v:%d failed", req.Src.Addr(), internalPort)
			return
		}
		res := make([]byte, 0, 16)
		res = append(res,
			0,     // version 0 (NAT-PMP)
			1+128, // response to op 1
			0, 0,  // result code success
		)
		res = binary.BigEndian.AppendUint32(res, uint32(time.Now().Unix()))
		res = binary.BigEndian.AppendUint16(res, internalPort)
		res = binary.BigEndian.AppendUint16(res, gotPort)
		res = binary.BigEndian.AppendUint32(res, lifetimeSec)
		n.WriteUDPPacketNoNAT(UDPPacket{
			Src:     req.Dst,
			Dst:     req.Src,
			Payload: res,
		})
		return
	}

	n.logf("TODO: handle NAT-PMP packet % 02x", req.Payload)
}

// UDPPacket is a UDP packet.
//
// For the purposes of this project, a UDP packet
// (not a general IP packet) is the unit to be NAT'ed,
// as that's all that Tailscale uses.
type UDPPacket struct {
	Src     netip.AddrPort
	Dst     netip.AddrPort
	Payload []byte // everything after UDP header
}

func (s *Server) WriteStartingBanner(w io.Writer) {
	fmt.Fprintf(w, "vnet serving clients:\n")

	for _, n := range s.nodes {
		fmt.Fprintf(w, "  %v %15v (%v, %v)\n", n.mac, n.lanIP, n.net.wanIP4, n.net.natStyle.Load())
	}
}

type agentConn struct {
	node *node
	tc   *gonet.TCPConn
}

func (s *Server) addIdleAgentConn(ac *agentConn) {
	//log.Printf("got agent conn from %v", ac.node.mac)
	s.mu.Lock()
	defer s.mu.Unlock()

	s.agentConns.Make()
	s.agentConns.Add(ac)

	if waiter, ok := s.agentConnWaiter[ac.node]; ok {
		select {
		case waiter <- struct{}{}:
		default:
		}
	}
}

func (s *Server) takeAgentConn(ctx context.Context, n *node) (_ *agentConn, ok bool) {
	for {
		ac, ok := s.takeAgentConnOne(n)
		if ok {
			//log.Printf("got agent conn for %v", n.mac)
			return ac, true
		}
		s.mu.Lock()
		ready := make(chan struct{})
		mak.Set(&s.agentConnWaiter, n, ready)
		s.mu.Unlock()

		//log.Printf("waiting for agent conn for %v", n.mac)
		select {
		case <-ctx.Done():
			return nil, false
		case <-ready:
		case <-time.After(time.Second):
			// Try again regularly anyway, in case we have multiple clients
			// trying to hit the same node, or if a race means we weren't in the
			// select by the time addIdleAgentConn tried to signal us.
		}
	}
}

func (s *Server) takeAgentConnOne(n *node) (_ *agentConn, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ac := range s.agentConns {
		if ac.node == n {
			s.agentConns.Delete(ac)
			return ac, true
		}
	}
	return nil, false
}

type NodeAgentClient struct {
	*tailscale.LocalClient
	HTTPClient *http.Client
}

func (s *Server) NodeAgentDialer(n *Node) DialFunc {
	s.mu.Lock()
	defer s.mu.Unlock()

	if d, ok := s.agentDialer[n.n]; ok {
		return d
	}
	d := func(ctx context.Context, network, addr string) (net.Conn, error) {
		ac, ok := s.takeAgentConn(ctx, n.n)
		if !ok {
			return nil, ctx.Err()
		}
		return ac.tc, nil
	}
	mak.Set(&s.agentDialer, n.n, d)
	return d
}

func (s *Server) NodeAgentClient(n *Node) *NodeAgentClient {
	d := s.NodeAgentDialer(n)
	return &NodeAgentClient{
		LocalClient: &tailscale.LocalClient{
			UseSocketOnly: true,
			OmitAuth:      true,
			Dial:          d,
		},
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				DialContext: d,
			},
		},
	}
}

// EnableHostFirewall enables the host's stateful firewall.
func (c *NodeAgentClient) EnableHostFirewall(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://unused/fw", nil)
	if err != nil {
		return err
	}
	res, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	all, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		return fmt.Errorf("unexpected status code %v: %s", res.Status, all)
	}
	return nil
}
