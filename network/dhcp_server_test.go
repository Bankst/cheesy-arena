// Copyright 2024 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)

package network

import (
	"net"
	"testing"
	"time"

	"github.com/Team254/cheesy-arena/model"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/stretchr/testify/assert"
)

func TestDhcpServerConfigurePools(t *testing.T) {
	ds := NewDhcpServer("10.0.100.5")

	// No pools initially.
	assert.Equal(t, 0, len(ds.pools))

	// Configure pools for a partial lineup.
	ds.ConfigureTeamPools([6]*model.Team{{Id: 254}, nil, nil, nil, nil, nil})
	assert.Equal(t, 1, len(ds.pools))
	gatewayKey := ipToUint32(net.IPv4(10, 2, 54, 4).To4())
	pool, ok := ds.pools[gatewayKey]
	assert.True(t, ok)
	assert.Equal(t, net.IPv4(10, 2, 54, 0).To4(), pool.network)
	assert.Equal(t, net.IPv4(10, 2, 54, 4).To4(), pool.gateway)

	// Configure pools for a full lineup.
	ds.ConfigureTeamPools([6]*model.Team{
		{Id: 1114}, {Id: 254}, {Id: 296}, {Id: 1503}, {Id: 1678}, {Id: 1538},
	})
	assert.Equal(t, 6, len(ds.pools))
}

func TestDhcpServerAllocateIp(t *testing.T) {
	ds := NewDhcpServer("10.0.100.5")
	ds.ConfigureTeamPools([6]*model.Team{{Id: 254}, nil, nil, nil, nil, nil})

	gatewayKey := ipToUint32(net.IPv4(10, 2, 54, 4).To4())
	pool := ds.pools[gatewayKey]

	// Allocate first IP.
	ip1 := ds.allocateIp("aa:bb:cc:dd:ee:01", pool)
	assert.Equal(t, net.IPv4(10, 2, 54, 20).To4(), ip1)

	// Same MAC gets the same IP.
	ip1Again := ds.allocateIp("aa:bb:cc:dd:ee:01", pool)
	assert.Equal(t, ip1, ip1Again)

	// Different MAC gets a different IP.
	ip2 := ds.allocateIp("aa:bb:cc:dd:ee:02", pool)
	assert.Equal(t, net.IPv4(10, 2, 54, 21).To4(), ip2)

	// Reconfiguring pools clears leases.
	ds.ConfigureTeamPools([6]*model.Team{{Id: 254}, nil, nil, nil, nil, nil})
	pool = ds.pools[gatewayKey]
	ip3 := ds.allocateIp("aa:bb:cc:dd:ee:03", pool)
	assert.Equal(t, net.IPv4(10, 2, 54, 20).To4(), ip3)
}

func TestDhcpServerHandleRelayedPacket(t *testing.T) {
	ds := NewDhcpServer("10.0.100.5")
	ds.ConfigureTeamPools([6]*model.Team{{Id: 254}, nil, nil, nil, nil, nil})

	// Start a UDP listener to act as the DHCP server.
	serverAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
	conn, err := net.ListenPacket("udp4", serverAddr.String())
	assert.Nil(t, err)
	defer conn.Close()
	ds.conn = conn

	// Create a fake relayed DHCP DISCOVER.
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:01")
	discover, err := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeDiscover),
		dhcpv4.WithHwAddr(mac),
	)
	assert.Nil(t, err)
	discover.GatewayIPAddr = net.IPv4(10, 2, 54, 4)

	// Open a "relay agent" socket to receive the reply.
	relayAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
	relayConn, err := net.ListenPacket("udp4", relayAddr.String())
	assert.Nil(t, err)
	defer relayConn.Close()

	// Override the gateway to point to our test relay socket.
	discover.GatewayIPAddr = net.IPv4(127, 0, 0, 1)
	// We need the pool to be keyed on 127.0.0.1 for this test.
	localKey := ipToUint32(net.IPv4(127, 0, 0, 1).To4())
	ds.pools[localKey] = ds.pools[ipToUint32(net.IPv4(10, 2, 54, 4).To4())]

	// Send the discover.
	ds.handlePacket(relayConn.LocalAddr(), discover)

	// Read the reply from the relay socket.
	relayConn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1500)
	n, _, err := relayConn.ReadFrom(buf)
	assert.Nil(t, err)

	reply, err := dhcpv4.FromBytes(buf[:n])
	assert.Nil(t, err)
	assert.Equal(t, dhcpv4.MessageTypeOffer, reply.MessageType())
	assert.False(t, reply.YourIPAddr.IsUnspecified())
}

func TestDhcpServerPoolExhaustion(t *testing.T) {
	ds := NewDhcpServer("10.0.100.5")
	ds.ConfigureTeamPools([6]*model.Team{{Id: 254}, nil, nil, nil, nil, nil})

	gatewayKey := ipToUint32(net.IPv4(10, 2, 54, 4).To4())
	pool := ds.pools[gatewayKey]

	// Allocate all IPs in the pool.
	for i := dhcpPoolStartAddr; i <= dhcpPoolEndAddr; i++ {
		mac := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, byte(i >> 8), byte(i)}
		ip := ds.allocateIp(mac.String(), pool)
		assert.NotNil(t, ip)
	}

	// Next allocation should fail.
	ip := ds.allocateIp("ff:ff:ff:ff:ff:ff", pool)
	assert.Nil(t, ip)
}

func TestTeamIpHelpers(t *testing.T) {
	team := &model.Team{Id: 254}
	assert.Equal(t, net.IPv4(10, 2, 54, 4).To4(), teamGatewayIp(team))
	assert.Equal(t, net.IPv4(10, 2, 54, 0).To4(), teamNetworkIp(team))

	team = &model.Team{Id: 1114}
	assert.Equal(t, net.IPv4(10, 11, 14, 4).To4(), teamGatewayIp(team))
	assert.Equal(t, net.IPv4(10, 11, 14, 0).To4(), teamNetworkIp(team))
}
