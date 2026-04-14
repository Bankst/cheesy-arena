// Copyright 2024 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)
//
// Methods for configuring an HP ProCurve 2810-series switch for team VLANs.
// Unlike the Cisco Catalyst 3500, the ProCurve lacks a built-in DHCP server,
// so it configures ip helper-address on each VLAN to relay DHCP to the arena server.

package network

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Team254/cheesy-arena/model"
)

type HPSwitch struct {
	address               string
	port                  int
	password              string
	mutex                 sync.Mutex
	configBackoffDuration time.Duration
	configPauseDuration   time.Duration
	status                string
	dhcpServerAddress     string
}

func NewHPSwitch(address, password, dhcpServerAddress string) *HPSwitch {
	return &HPSwitch{
		address:               address,
		port:                  switchTelnetPort,
		password:              password,
		configBackoffDuration: switchConfigBackoffDurationSec * time.Second,
		configPauseDuration:   switchConfigPauseDurationSec * time.Second,
		status:                "UNKNOWN",
		dhcpServerAddress:     dhcpServerAddress,
	}
}

func (sw *HPSwitch) GetStatus() string {
	return sw.status
}

// Sets up wired networks for the given set of teams.
func (sw *HPSwitch) ConfigureTeamEthernet(teams [6]*model.Team) error {
	sw.mutex.Lock()
	defer sw.mutex.Unlock()
	sw.status = "CONFIGURING"

	// Remove old VLAN IP addresses and helper-addresses to reset the switch state.
	removeCommand := ""
	for vlan := 10; vlan <= 60; vlan += 10 {
		removeCommand += fmt.Sprintf("vlan %d\nno ip address\nno ip helper-address\nexit\n", vlan)
	}
	_, err := sw.runConfigCommand(removeCommand)
	if err != nil {
		sw.status = "ERROR"
		return err
	}
	time.Sleep(sw.configPauseDuration)

	// Configure the new VLAN IP addresses and DHCP relay.
	addCommand := ""
	addTeamVlan := func(team *model.Team, vlan int) {
		if team == nil {
			return
		}
		teamPartialIp := fmt.Sprintf("%d.%d", team.Id/100, team.Id%100)
		addCommand += fmt.Sprintf(
			"vlan %d\nip address 10.%s.%d 255.255.255.0\nip helper-address %s\nexit\n",
			vlan,
			teamPartialIp,
			switchTeamGatewayAddress,
			sw.dhcpServerAddress,
		)
	}
	addTeamVlan(teams[0], red1Vlan)
	addTeamVlan(teams[1], red2Vlan)
	addTeamVlan(teams[2], red3Vlan)
	addTeamVlan(teams[3], blue1Vlan)
	addTeamVlan(teams[4], blue2Vlan)
	addTeamVlan(teams[5], blue3Vlan)
	if len(addCommand) > 0 {
		_, err = sw.runConfigCommand(addCommand)
		if err != nil {
			sw.status = "ERROR"
			return err
		}
	}

	time.Sleep(sw.configBackoffDuration)

	sw.status = "ACTIVE"
	return nil
}

// Logs into the switch via Telnet and runs the given command in user exec mode. Reads the output and
// returns it as a string.
func (sw *HPSwitch) runCommand(command string) (string, error) {
	conn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", sw.address, sw.port))
	if err != nil {
		return "", err
	}
	defer conn.Close()

	// The ProCurve login sequence: press any key, then password, then commands.
	writer := bufio.NewWriter(conn)
	_, err = writer.WriteString(
		fmt.Sprintf(
			"\n%s\nno page\n%sexit\n", sw.password, command,
		),
	)
	if err != nil {
		return "", err
	}
	err = writer.Flush()
	if err != nil {
		return "", err
	}

	var reader bytes.Buffer
	_, err = reader.ReadFrom(conn)
	if err != nil {
		return "", err
	}
	return reader.String(), nil
}

// Logs into the switch via Telnet and runs the given command in global configuration mode. Reads the output
// and returns it as a string.
func (sw *HPSwitch) runConfigCommand(command string) (string, error) {
	return sw.runCommand(fmt.Sprintf("configure terminal\n%sexit\nwrite memory\n", command))
}
