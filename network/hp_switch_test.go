// Copyright 2024 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)

package network

import (
	"testing"
	"time"

	"github.com/Team254/cheesy-arena/model"
	"github.com/stretchr/testify/assert"
)

func TestConfigureHPSwitch(t *testing.T) {
	sw := NewHPSwitch("127.0.0.1", "password", "10.0.100.5")
	assert.Equal(t, "UNKNOWN", sw.GetStatus())
	sw.port = 9060
	sw.configBackoffDuration = time.Millisecond
	sw.configPauseDuration = time.Millisecond
	var command1, command2 string

	expectedResetCommand := "\npassword\nno page\nconfigure terminal\n" +
		"vlan 10\nno ip address\nno ip helper-address\nexit\n" +
		"vlan 20\nno ip address\nno ip helper-address\nexit\n" +
		"vlan 30\nno ip address\nno ip helper-address\nexit\n" +
		"vlan 40\nno ip address\nno ip helper-address\nexit\n" +
		"vlan 50\nno ip address\nno ip helper-address\nexit\n" +
		"vlan 60\nno ip address\nno ip helper-address\nexit\n" +
		"exit\nwrite memory\nexit\n"

	// Should remove all previous VLAN IPs and do nothing else if current configuration is blank.
	mockTelnet(t, sw.port, &command1, &command2)
	assert.Nil(t, sw.ConfigureTeamEthernet([6]*model.Team{nil, nil, nil, nil, nil, nil}))
	assert.Equal(t, expectedResetCommand, command1)
	assert.Equal(t, "", command2)
	assert.Equal(t, "ACTIVE", sw.GetStatus())

	// Should configure one team if only one is present.
	sw.port += 1
	mockTelnet(t, sw.port, &command1, &command2)
	assert.Nil(t, sw.ConfigureTeamEthernet([6]*model.Team{nil, nil, nil, nil, {Id: 254}, nil}))
	assert.Equal(t, expectedResetCommand, command1)
	assert.Equal(
		t,
		"\npassword\nno page\nconfigure terminal\n"+
			"vlan 50\nip address 10.2.54.4 255.255.255.0\nip helper-address 10.0.100.5\nexit\n"+
			"exit\nwrite memory\nexit\n",
		command2,
	)

	// Should configure all teams if all are present.
	sw.port += 1
	mockTelnet(t, sw.port, &command1, &command2)
	assert.Nil(
		t,
		sw.ConfigureTeamEthernet([6]*model.Team{{Id: 1114}, {Id: 254}, {Id: 296}, {Id: 1503}, {Id: 1678}, {Id: 1538}}),
	)
	assert.Equal(t, expectedResetCommand, command1)
	assert.Equal(
		t,
		"\npassword\nno page\nconfigure terminal\n"+
			"vlan 10\nip address 10.11.14.4 255.255.255.0\nip helper-address 10.0.100.5\nexit\n"+
			"vlan 20\nip address 10.2.54.4 255.255.255.0\nip helper-address 10.0.100.5\nexit\n"+
			"vlan 30\nip address 10.2.96.4 255.255.255.0\nip helper-address 10.0.100.5\nexit\n"+
			"vlan 40\nip address 10.15.3.4 255.255.255.0\nip helper-address 10.0.100.5\nexit\n"+
			"vlan 50\nip address 10.16.78.4 255.255.255.0\nip helper-address 10.0.100.5\nexit\n"+
			"vlan 60\nip address 10.15.38.4 255.255.255.0\nip helper-address 10.0.100.5\nexit\n"+
			"exit\nwrite memory\nexit\n",
		command2,
	)
}
