// Package radvd implements IPv6 router advertisments.
package radvd

import (
	"encoding/binary"
	"log"
	"net"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv6"
)

type Server struct {
	pc *ipv6.PacketConn

	mu       sync.Mutex
	prefixes []net.IPNet
	iface    *net.Interface
}

func NewServer() (*Server, error) {
	return &Server{}, nil
}

func (s *Server) SetPrefixes(prefixes []net.IPNet) {
	s.mu.Lock()
	s.prefixes = prefixes
	s.mu.Unlock()
	if s.iface != nil {
		s.sendAdvertisement(nil)
	}
}

func (s *Server) ListenAndServe(ifname string) error {
	var err error
	s.iface, err = net.InterfaceByName(ifname)
	if err != nil {
		return err
	}

	// TODO(correctness): would it be better to listen on
	// net.IPv6linklocalallrouters? Just specifying that results in an error,
	// though.
	conn, err := net.ListenIP("ip6:ipv6-icmp", &net.IPAddr{net.IPv6unspecified, ""})
	if err != nil {
		return err
	}
	defer conn.Close()
	s.pc = ipv6.NewPacketConn(conn)
	s.pc.SetHopLimit(255) // as per RFC 4861, section 4.1

	var filter ipv6.ICMPFilter
	filter.SetAll(true)
	filter.Accept(ipv6.ICMPTypeRouterSolicitation)
	if err := s.pc.SetICMPFilter(&filter); err != nil {
		return err
	}

	go func() {
		for {
			s.sendAdvertisement(nil) // TODO: handle error
			time.Sleep(1 * time.Minute)
		}
	}()

	// A 512 bytes buffer is sufficient for router solicitation packets, which
	// are basically empty.
	buf := make([]byte, 512)
	for {
		n, _, addr, err := s.pc.ReadFrom(buf)
		if err != nil {
			return err
		}
		// TODO: isn’t this guaranteed by the filter above?
		if n == 0 ||
			ipv6.ICMPType(buf[0]) != ipv6.ICMPTypeRouterSolicitation {
			continue
		}
		if err := s.sendAdvertisement(addr); err != nil {
			return err
		}
	}

	return nil
}

type sourceLinkLayerAddress struct {
	address net.HardwareAddr
}

func (o sourceLinkLayerAddress) Marshal() layers.ICMPv6Option {
	return layers.ICMPv6Option{
		Type: layers.ICMPv6OptSourceAddress,
		Data: o.address,
	}
}

type mtu struct {
	mtu uint32
}

func (o mtu) Marshal() layers.ICMPv6Option {
	buf := make([]byte, 6)
	// First 2 bytes are reserved
	binary.BigEndian.PutUint32(buf[2:], o.mtu)
	return layers.ICMPv6Option{
		Type: layers.ICMPv6OptMTU,
		Data: buf,
	}
}

type prefixInfo struct {
	prefixLength      byte
	flags             byte   // TODO: enum for values
	validLifetime     uint32 // seconds
	preferredLifetime uint32 // seconds
	prefix            [16]byte
}

func (o prefixInfo) Marshal() layers.ICMPv6Option {
	buf := make([]byte, 30)
	buf[0] = o.prefixLength
	buf[1] = o.flags
	binary.BigEndian.PutUint32(buf[2:], o.validLifetime)
	binary.BigEndian.PutUint32(buf[6:], o.preferredLifetime)
	// 4 bytes reserved
	copy(buf[14:], o.prefix[:])
	return layers.ICMPv6Option{
		Type: layers.ICMPv6OptPrefixInfo,
		Data: buf,
	}
}

type rdnss struct {
	lifetime uint32 // seconds
	server   [16]byte
}

func (o rdnss) Marshal() layers.ICMPv6Option {
	buf := make([]byte, 22)
	// 2 bytes reserved
	binary.BigEndian.PutUint32(buf[2:], o.lifetime)
	copy(buf[6:], o.server[:])
	return layers.ICMPv6Option{
		Type: 25, // TODO: Recursive DNS Server
		Data: buf,
	}
}

func (s *Server) sendAdvertisement(addr net.Addr) error {
	if addr == nil {
		addr = &net.IPAddr{net.IPv6linklocalallnodes, s.iface.Name}
	}
	// TODO: cache the packet
	msgbody := []byte{
		0x40,       // hop limit: 64
		0x80,       // flags: managed address configuration
		0x07, 0x08, // router lifetime (s): 1800
		0x00, 0x00, 0x00, 0x00, // reachable time (ms): 0
		0x00, 0x00, 0x00, 0x00, // retrans time (ms): 0
	}

	options := layers.ICMPv6Options{
		(sourceLinkLayerAddress{address: s.iface.HardwareAddr}).Marshal(),
		(mtu{mtu: uint32(s.iface.MTU)}).Marshal(),
	}
	s.mu.Lock()
	for _, prefix := range s.prefixes {
		ones, _ := prefix.Mask.Size()
		// Use the first /64 subnet within larger prefixes
		if ones < 64 {
			ones = 64
		}

		var net [16]byte
		copy(net[:], prefix.IP)
		options = append(options, (prefixInfo{
			prefixLength:      byte(ones),
			flags:             0xc0, // TODO
			validLifetime:     7200, // seconds
			preferredLifetime: 1800, // seconds
			prefix:            net,
		}).Marshal())
	}
	if len(s.prefixes) > 0 {
		prefix := s.prefixes[0]
		var server [16]byte
		copy(server[:], prefix.IP)
		// pick the first address of the prefix, e.g. address 2a02:168:4a00::1
		// for prefix 2a02:168:4a00::/48
		server[len(server)-1] = 1
		options = append(options, (rdnss{
			lifetime: 1800, // seconds
			server:   server,
		}).Marshal())
	}
	s.mu.Unlock()

	buf := gopacket.NewSerializeBuffer()
	if err := options.SerializeTo(buf, gopacket.SerializeOptions{FixLengths: true}); err != nil {
		return err
	}
	msgbody = append(msgbody, buf.Bytes()...)

	msg := &icmp.Message{
		Type:     ipv6.ICMPTypeRouterAdvertisement,
		Code:     0,
		Checksum: 0, // calculated by the kernel
		Body:     &icmp.DefaultMessageBody{msgbody}}
	mb, err := msg.Marshal(nil)
	if err != nil {
		return err
	}
	log.Printf("sending to %s (%#v)", addr, addr)
	if _, err := s.pc.WriteTo(mb, nil, addr); err != nil {
		return err
	}
	return nil
}
