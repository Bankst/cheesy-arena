// Copyright 2024 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)
//
// Embedded DHCPv4 server for use with switches that lack a built-in DHCP server (e.g., HP ProCurve).
// Handles DHCP relay (ip helper-address) traffic from team VLANs and assigns IPs from per-team pools.

package network

import (
	"log"
	"net"
	"sync"
	"time"

	"github.com/Team254/cheesy-arena/model"
	"github.com/insomniacslk/dhcp/dhcpv4"
)

const (
	dhcpServerPort    = 67
	dhcpLeaseTimeSec  = 7 * 24 * 60 * 60 // 7 days
	dhcpPoolStartAddr = 20               // First assignable host address
	dhcpPoolEndAddr   = 199              // Last assignable host address
)

type DhcpServer struct {
	mutex    sync.Mutex
	pools    map[uint32]*dhcpPool  // gateway IP (network byte order) -> pool
	leases   map[string]*dhcpLease // MAC address string -> lease
	serverIp net.IP
	conn     net.PacketConn
}

type dhcpPool struct {
	network net.IP
	gateway net.IP
	nextIp  int // next host address offset to try (dhcpPoolStartAddr to dhcpPoolEndAddr)
}

type dhcpLease struct {
	ip     net.IP
	pool   *dhcpPool
	expiry time.Time
}

func NewDhcpServer(serverIp string) *DhcpServer {
	return &DhcpServer{
		pools:    make(map[uint32]*dhcpPool),
		leases:   make(map[string]*dhcpLease),
		serverIp: net.ParseIP(serverIp).To4(),
	}
}

// ConfigureTeamPools updates the DHCP pools to match the current team lineup.
func (ds *DhcpServer) ConfigureTeamPools(teams [6]*model.Team) {
	ds.mutex.Lock()
	defer ds.mutex.Unlock()

	// Clear existing pools and leases.
	ds.pools = make(map[uint32]*dhcpPool)
	ds.leases = make(map[string]*dhcpLease)

	for _, team := range teams {
		if team == nil {
			continue
		}
		gateway := teamGatewayIp(team)
		networkIp := teamNetworkIp(team)
		key := ipToUint32(gateway)
		ds.pools[key] = &dhcpPool{
			network: networkIp,
			gateway: gateway,
			nextIp:  dhcpPoolStartAddr,
		}
	}
}

// Run starts the DHCP server. This blocks and should be called as a goroutine.
func (ds *DhcpServer) Run() {
	addr := &net.UDPAddr{IP: net.IPv4zero, Port: dhcpServerPort}
	var err error
	ds.conn, err = net.ListenPacket("udp4", addr.String())
	if err != nil {
		log.Printf("Failed to start DHCP server: %v", err)
		return
	}
	defer ds.conn.Close()
	log.Printf("DHCP server listening on %s", addr)

	buf := make([]byte, 1500)
	for {
		n, peer, err := ds.conn.ReadFrom(buf)
		if err != nil {
			log.Printf("DHCP server read error: %v", err)
			return
		}
		msg, err := dhcpv4.FromBytes(buf[:n])
		if err != nil {
			log.Printf("DHCP server: invalid packet from %v: %v", peer, err)
			continue
		}
		go ds.handlePacket(peer, msg)
	}
}

func (ds *DhcpServer) handlePacket(peer net.Addr, req *dhcpv4.DHCPv4) {
	if req.OpCode != dhcpv4.OpcodeBootRequest {
		return
	}

	msgType := req.MessageType()
	switch msgType {
	case dhcpv4.MessageTypeDiscover:
		ds.handleDiscover(peer, req)
	case dhcpv4.MessageTypeRequest:
		ds.handleRequest(peer, req)
	case dhcpv4.MessageTypeRelease:
		ds.handleRelease(req)
	}
}

func (ds *DhcpServer) handleDiscover(peer net.Addr, req *dhcpv4.DHCPv4) {
	ds.mutex.Lock()
	defer ds.mutex.Unlock()

	pool := ds.findPool(req)
	if pool == nil {
		log.Printf("DHCP server: no pool for request from %s (giaddr=%s)", req.ClientHWAddr, req.GatewayIPAddr)
		return
	}

	ip := ds.allocateIp(req.ClientHWAddr.String(), pool)
	if ip == nil {
		log.Printf("DHCP server: pool exhausted for network %s", pool.network)
		return
	}

	reply, err := dhcpv4.NewReplyFromRequest(req,
		dhcpv4.WithMessageType(dhcpv4.MessageTypeOffer),
		dhcpv4.WithYourIP(ip),
		dhcpv4.WithServerIP(ds.serverIp),
		dhcpv4.WithOption(dhcpv4.OptSubnetMask(net.CIDRMask(24, 32))),
		dhcpv4.WithOption(dhcpv4.OptRouter(pool.gateway)),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(ds.serverIp)),
		dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(dhcpLeaseTimeSec*time.Second)),
	)
	if err != nil {
		log.Printf("DHCP server: failed to create OFFER: %v", err)
		return
	}

	ds.sendReply(peer, req, reply)
}

func (ds *DhcpServer) handleRequest(peer net.Addr, req *dhcpv4.DHCPv4) {
	ds.mutex.Lock()
	defer ds.mutex.Unlock()

	pool := ds.findPool(req)
	if pool == nil {
		log.Printf("DHCP server: no pool for REQUEST from %s (giaddr=%s)", req.ClientHWAddr, req.GatewayIPAddr)
		return
	}

	ip := ds.allocateIp(req.ClientHWAddr.String(), pool)
	if ip == nil {
		log.Printf("DHCP server: pool exhausted for network %s", pool.network)
		return
	}

	reply, err := dhcpv4.NewReplyFromRequest(req,
		dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
		dhcpv4.WithYourIP(ip),
		dhcpv4.WithServerIP(ds.serverIp),
		dhcpv4.WithOption(dhcpv4.OptSubnetMask(net.CIDRMask(24, 32))),
		dhcpv4.WithOption(dhcpv4.OptRouter(pool.gateway)),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(ds.serverIp)),
		dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(dhcpLeaseTimeSec*time.Second)),
	)
	if err != nil {
		log.Printf("DHCP server: failed to create ACK: %v", err)
		return
	}

	ds.sendReply(peer, req, reply)
}

func (ds *DhcpServer) handleRelease(req *dhcpv4.DHCPv4) {
	ds.mutex.Lock()
	defer ds.mutex.Unlock()

	mac := req.ClientHWAddr.String()
	delete(ds.leases, mac)
}

// findPool returns the pool matching the request's giaddr (relay agent gateway) or client subnet.
func (ds *DhcpServer) findPool(req *dhcpv4.DHCPv4) *dhcpPool {
	// For relayed packets, giaddr identifies the VLAN.
	if !req.GatewayIPAddr.IsUnspecified() {
		key := ipToUint32(req.GatewayIPAddr.To4())
		if pool, ok := ds.pools[key]; ok {
			return pool
		}
	}
	return nil
}

// allocateIp returns an IP for the given MAC, reusing an existing lease or allocating a new one.
func (ds *DhcpServer) allocateIp(mac string, pool *dhcpPool) net.IP {
	// Reuse existing lease for this MAC if it's in the same pool.
	if lease, ok := ds.leases[mac]; ok && lease.pool == pool {
		lease.expiry = time.Now().Add(dhcpLeaseTimeSec * time.Second)
		return lease.ip
	}

	// Expire old leases.
	now := time.Now()
	for k, lease := range ds.leases {
		if lease.expiry.Before(now) {
			delete(ds.leases, k)
		}
	}

	// Find an unused address in the pool range.
	for attempts := 0; attempts <= dhcpPoolEndAddr-dhcpPoolStartAddr; attempts++ {
		candidateIp := make(net.IP, 4)
		copy(candidateIp, pool.network.To4())
		candidateIp[3] = byte(pool.nextIp)

		pool.nextIp++
		if pool.nextIp > dhcpPoolEndAddr {
			pool.nextIp = dhcpPoolStartAddr
		}

		if !ds.isIpInUse(candidateIp) {
			lease := &dhcpLease{
				ip:     candidateIp,
				pool:   pool,
				expiry: time.Now().Add(dhcpLeaseTimeSec * time.Second),
			}
			ds.leases[mac] = lease
			return candidateIp
		}
	}
	return nil
}

func (ds *DhcpServer) isIpInUse(ip net.IP) bool {
	for _, lease := range ds.leases {
		if lease.ip.Equal(ip) {
			return true
		}
	}
	return false
}

// sendReply sends the DHCP reply back to the relay agent or directly to the client.
func (ds *DhcpServer) sendReply(peer net.Addr, req, reply *dhcpv4.DHCPv4) {
	var dst net.Addr
	if !req.GatewayIPAddr.IsUnspecified() {
		// Relayed request: send reply back to the relay agent on port 67.
		dst = &net.UDPAddr{IP: req.GatewayIPAddr, Port: dhcpServerPort}
	} else {
		dst = peer
	}

	_, err := ds.conn.WriteTo(reply.ToBytes(), dst)
	if err != nil {
		log.Printf("DHCP server: failed to send reply to %v: %v", dst, err)
	}
}

// teamGatewayIp returns the gateway IP for a team's VLAN (10.TE.AM.4).
func teamGatewayIp(team *model.Team) net.IP {
	return net.IPv4(10, byte(team.Id/100), byte(team.Id%100), switchTeamGatewayAddress).To4()
}

// teamNetworkIp returns the network IP for a team's VLAN (10.TE.AM.0).
func teamNetworkIp(team *model.Team) net.IP {
	return net.IPv4(10, byte(team.Id/100), byte(team.Id%100), 0).To4()
}

func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}
