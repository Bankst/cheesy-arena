// Mock FRC driver stations for testing cheesy-arena match flow without real robots.
//
// For each team specified on the command line this tool:
//
//   - opens a TCP connection to the arena's DS listener (port 1750), sends the
//     initial registration packet, reads the station-assignment reply, and
//     parses every tagged TCP packet the server sends back (game-data pushes
//     mainly) so we can surface them;
//   - sends a length-1 "DS keepalive" packet (type 29) every ~1 s so the
//     server's 5-second TCP read deadline does not expire;
//   - sends a UDP status packet (DS/Radio/Rio/Robot linked, battery 12.5 V) to
//     the server's DS receive port (1160) every 500 ms so the arena considers
//     the robot connected and ready.
//
// A shared wildcard UDP listener on the server-push ports (1120 and 1121)
// drains the 22-byte control packets the arena sends at each DS, parses them,
// and surfaces the fields (auto/enabled/estop/astop bits, match type+number,
// remaining seconds in the current period). Draining those packets also keeps
// server-side sendto calls from failing with ECONNREFUSED.
//
// By default the tool streams decoded events to stdout as log lines. Pass
// -tui for a live ncurses-style per-station table (pure ANSI, no deps).
//
// Run (default targets the 10.0.100.5 loopback that ./fake_fms_ip.sh up creates):
//
//	go run ./tools/mock-driver-stations 111 222 333 444 555 666
//	go run ./tools/mock-driver-stations -tui 111 222 333 444 555 666
//
// Point elsewhere with -server if the arena binds to a different address.
// Load the matching teams into a Quick Play match first. NetworkSecurityEnabled
// must be disabled on the arena (the default for home use) since every mock team
// shares the host's source IP.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	dsTcpPort              = 1750
	dsUdpReceivePort       = 1160
	udpPacketIntervalMs    = 500
	tcpKeepaliveIntervalMs = 1000
	batteryVoltage         = 12.5
	tuiRedrawIntervalMs    = 200
)

// Ports the arena may dial to push control packets back at the DS. 1121 is the
// classic FMS port; 1120 is used when the server has UseLiteUdpPort enabled.
var serverSendToPorts = []int{1121, 1120}

// Shared station-byte -> team-number map so the drain listener can attach
// human-readable team IDs to the control packets it decodes. Populated on
// TCP registration, cleared on disconnect.
var (
	stationTeamMu sync.RWMutex
	stationTeam   = map[byte]int{}
)

func setStationTeam(station byte, team int) {
	stationTeamMu.Lock()
	stationTeam[station] = team
	stationTeamMu.Unlock()
}

func clearStationTeam(station byte, team int) {
	stationTeamMu.Lock()
	if stationTeam[station] == team {
		delete(stationTeam, station)
	}
	stationTeamMu.Unlock()
}

func teamForStation(station byte) int {
	stationTeamMu.RLock()
	defer stationTeamMu.RUnlock()
	return stationTeam[station]
}

// stationState holds the latest observed status per station for the TUI. All
// fields are read/written under view.mu.
type stationState struct {
	Team         int
	Station      string
	StationByte  byte
	TcpUp        bool
	UdpSent      int
	KeepSent     int
	Auto         bool
	Enabled      bool
	EStop        bool
	AStop        bool
	MatchType    byte
	MatchNum     uint16
	Remaining    uint16
	LastGameData string
	LastEventAt  time.Time
	LastEvent    string
}

type viewModel struct {
	mu       sync.Mutex
	stations map[byte]*stationState // key = station byte (0-5)
	events   []string
}

func newViewModel() *viewModel {
	return &viewModel{stations: map[byte]*stationState{}}
}

func (v *viewModel) getOrCreate(stationByte byte) *stationState {
	s, ok := v.stations[stationByte]
	if !ok {
		s = &stationState{StationByte: stationByte, Station: stationNameFromByte(stationByte)}
		v.stations[stationByte] = s
	}
	return s
}

func (v *viewModel) pushEvent(msg string) {
	const maxEvents = 8
	v.events = append(v.events, fmt.Sprintf("%s  %s", time.Now().Format("15:04:05.000"), msg))
	if len(v.events) > maxEvents {
		v.events = v.events[len(v.events)-maxEvents:]
	}
}

var view = newViewModel()
var tuiEnabled bool

// logf routes a formatted message: in TUI mode the event is shown in the event
// pane, otherwise it goes to the standard logger.
func logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if tuiEnabled {
		view.mu.Lock()
		view.pushEvent(msg)
		view.mu.Unlock()
		return
	}
	log.Print(msg)
}

func main() {
	server := flag.String("server", "10.0.100.5", "Cheesy Arena server host or IP (must match network.ServerIpAddress)")
	batteryFlag := flag.Float64("battery", batteryVoltage, "reported battery voltage (must exceed 8 V for \"robot OK\")")
	tui := flag.Bool("tui", false, "render a live ANSI dashboard instead of streaming logs")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [-server host] [-tui] team1 team2 ... teamN\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	teams := make([]int, 0, flag.NArg())
	for _, arg := range flag.Args() {
		id, err := strconv.Atoi(arg)
		if err != nil || id <= 0 {
			log.Fatalf("invalid team number %q", arg)
		}
		teams = append(teams, id)
	}

	tuiEnabled = *tui
	if tuiEnabled {
		// Redirect log.* output to stderr only (it would otherwise scramble the
		// TUI). Callers can redirect stderr to a file if they want the raw log.
		log.SetFlags(log.LstdFlags)
		log.SetOutput(io.Discard)
	}

	stop := make(chan struct{})
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		if tuiEnabled {
			// Show cursor + newline so the shell prompt lands cleanly.
			fmt.Print("\x1b[?25h\n")
		}
		close(stop)
	}()

	if err := startUdpDrainListener(*server, stop); err != nil {
		log.Fatalf("failed to start UDP drain listener: %v", err)
	}
	logf("draining server control packets on :%d and :%d", serverSendToPorts[0], serverSendToPorts[1])

	if tuiEnabled {
		go runRenderer(stop)
	}

	var wg sync.WaitGroup
	for _, team := range teams {
		wg.Add(1)
		go func(teamId int) {
			defer wg.Done()
			runMockStation(*server, teamId, *batteryFlag, stop)
		}(team)
	}
	wg.Wait()

	if tuiEnabled {
		fmt.Print("\x1b[?25h\n")
	}
}

// startUdpDrainListener binds wildcard UDP sockets on every port the arena may
// dial back to the DS (1121 classic FMS, 1120 for UseLiteUdpPort). All mock
// teams share these listeners because they all look like the same host to the
// server. Binding to 0.0.0.0 avoids routing-table oddities leaving us silently
// deaf.
func startUdpDrainListener(server string, stop <-chan struct{}) error {
	_ = server
	for _, port := range serverSendToPorts {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
		if err != nil {
			return fmt.Errorf("listen :%d: %w", port, err)
		}
		go func(c *net.UDPConn) {
			<-stop
			c.Close()
		}(conn)
		go runControlPacketListener(conn)
	}
	return nil
}

func runControlPacketListener(conn *net.UDPConn) {
	buf := make([]byte, 2048)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 22 {
			continue
		}
		stationByte := buf[5]
		statusByte := buf[3]
		matchType := buf[6]
		matchNum := binary.BigEndian.Uint16(buf[7:9])
		remaining := binary.BigEndian.Uint16(buf[20:22])

		auto := statusByte&0x02 != 0
		enabled := statusByte&0x04 != 0
		estop := statusByte&0x80 != 0
		astop := statusByte&0x40 != 0

		view.mu.Lock()
		s := view.getOrCreate(stationByte)
		prevEnabled := s.Enabled
		prevAuto := s.Auto
		prevEstop := s.EStop
		prevAstop := s.AStop
		prevMatchType := s.MatchType
		prevMatchNum := s.MatchNum
		s.Auto = auto
		s.Enabled = enabled
		s.EStop = estop
		s.AStop = astop
		s.MatchType = matchType
		s.MatchNum = matchNum
		s.Remaining = remaining
		if s.Team == 0 {
			s.Team = teamForStation(stationByte)
		}
		// Emit an event only on field changes so the event pane does not scroll
		// constantly from per-packet remaining-time updates.
		if prevEnabled != enabled || prevAuto != auto || prevEstop != estop || prevAstop != astop ||
			prevMatchType != matchType || prevMatchNum != matchNum {
			mode := "teleop"
			if auto {
				mode = "auto"
			}
			state := "disabled"
			if enabled {
				state = "ENABLED"
			}
			view.pushEvent(fmt.Sprintf(
				"[fms] station=%s %s %s estop=%v astop=%v match=%s#%d",
				s.Station, mode, state, estop, astop, matchTypeName(matchType), matchNum,
			))
			if !tuiEnabled {
				log.Printf(
					"[fms] team=%d station=%s %s %s estop=%v astop=%v match=%s#%d remaining=%ds",
					s.Team, s.Station, mode, state, estop, astop, matchTypeName(matchType), matchNum, remaining,
				)
			}
		}
		view.mu.Unlock()
	}
}

func matchTypeName(b byte) string {
	switch b {
	case 0:
		return "test"
	case 1:
		return "practice"
	case 2:
		return "qual"
	case 3:
		return "playoff"
	}
	return fmt.Sprintf("unknown(%d)", b)
}

func runMockStation(server string, teamId int, battery float64, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}

		if err := connectAndServe(server, teamId, battery, stop); err != nil {
			logf("[team %d] disconnected: %v; reconnecting in 2s", teamId, err)
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		return
	}
}

func connectAndServe(server string, teamId int, battery float64, stop <-chan struct{}) error {
	tcpAddr := fmt.Sprintf("%s:%d", server, dsTcpPort)
	tcpConn, err := net.DialTimeout("tcp", tcpAddr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("tcp dial: %w", err)
	}
	defer tcpConn.Close()

	// Registration packet: length=3, type=24, team_hi, team_lo.
	reg := []byte{0x00, 0x03, 0x18, byte(teamId >> 8), byte(teamId & 0xff)}
	if _, err := tcpConn.Write(reg); err != nil {
		return fmt.Errorf("tcp write registration: %w", err)
	}

	// Read assignment reply: length=3, type=25, station, status.
	reply := make([]byte, 5)
	if _, err := io.ReadFull(tcpConn, reply); err != nil {
		return fmt.Errorf("tcp read assignment: %w", err)
	}
	if reply[2] != 25 {
		return fmt.Errorf("unexpected assignment packet type %d", reply[2])
	}
	stationByte := reply[3]
	stationName := stationNameFromByte(stationByte)
	statusCode := reply[4]
	if statusCode == 2 {
		return fmt.Errorf("server rejected team (not in current match)")
	}
	if stationName == "" {
		return fmt.Errorf("unknown station code %d", stationByte)
	}
	logf("[team %d] connected at station %s (status=%d)", teamId, stationName, statusCode)
	setStationTeam(stationByte, teamId)
	defer clearStationTeam(stationByte, teamId)

	view.mu.Lock()
	s := view.getOrCreate(stationByte)
	s.Team = teamId
	s.TcpUp = true
	view.mu.Unlock()
	defer func() {
		view.mu.Lock()
		if s, ok := view.stations[stationByte]; ok {
			s.TcpUp = false
		}
		view.mu.Unlock()
	}()

	udpAddr := fmt.Sprintf("%s:%d", server, dsUdpReceivePort)
	udpConn, err := net.Dial("udp4", udpAddr)
	if err != nil {
		return fmt.Errorf("udp dial: %w", err)
	}
	defer udpConn.Close()

	done := make(chan struct{})
	writeErr := make(chan error, 1)

	// Reader: parse tagged TCP packets pushed by the server.
	go func() {
		defer close(done)
		if err := readServerTcpPackets(tcpConn, teamId, stationByte); err != nil && err != io.EOF {
			logf("[team %d] tcp read ended: %v", teamId, err)
		}
	}()

	// TCP keepalive writer. The server applies a 5-second read deadline on every
	// tagged packet, so without periodic traffic it will drop us.
	keepalivePkt := []byte{0x00, 0x01, 29} // length=1, type=29 (DS keepalive).
	var keepaliveMu sync.Mutex
	writeKeepalive := func() error {
		keepaliveMu.Lock()
		defer keepaliveMu.Unlock()
		_, err := tcpConn.Write(keepalivePkt)
		return err
	}
	kaTicker := time.NewTicker(tcpKeepaliveIntervalMs * time.Millisecond)
	defer kaTicker.Stop()
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-done:
				return
			case <-kaTicker.C:
				if err := writeKeepalive(); err != nil {
					select {
					case writeErr <- fmt.Errorf("tcp keepalive: %w", err):
					default:
					}
					return
				}
				view.mu.Lock()
				if st, ok := view.stations[stationByte]; ok {
					st.KeepSent++
				}
				view.mu.Unlock()
			}
		}
	}()

	// UDP status sender.
	var packetCount uint16
	udpTicker := time.NewTicker(udpPacketIntervalMs * time.Millisecond)
	defer udpTicker.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-done:
			return fmt.Errorf("tcp closed by server")
		case err := <-writeErr:
			return err
		case <-udpTicker.C:
			packet := encodeStatusPacket(packetCount, teamId, battery)
			if _, err := udpConn.Write(packet); err != nil {
				return fmt.Errorf("udp write: %w", err)
			}
			packetCount++
			view.mu.Lock()
			if st, ok := view.stations[stationByte]; ok {
				st.UdpSent++
			}
			view.mu.Unlock()
		}
	}
}

// readServerTcpPackets parses each length-prefixed packet the server pushes and
// surfaces game-data packets (type 28). Unknown types are reported so protocol
// drift is visible.
func readServerTcpPackets(conn net.Conn, teamId int, stationByte byte) error {
	stationName := stationNameFromByte(stationByte)
	header := make([]byte, 2)
	for {
		if _, err := io.ReadFull(conn, header); err != nil {
			return err
		}
		length := int(header[0])<<8 | int(header[1])
		if length == 0 {
			continue
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(conn, body); err != nil {
			return err
		}
		packetType := body[0]
		payload := body[1:]
		switch packetType {
		case 28:
			dataLen := 0
			data := ""
			if len(payload) >= 1 {
				dataLen = int(payload[0])
				if 1+dataLen <= len(payload) {
					data = string(payload[1 : 1+dataLen])
				}
			}
			view.mu.Lock()
			if st, ok := view.stations[stationByte]; ok {
				st.LastGameData = data
			}
			view.mu.Unlock()
			logf("[fms->team %d %s] game data (len=%d): %q", teamId, stationName, dataLen, data)
		default:
			logf("[fms->team %d %s] unknown TCP packet type=%d len=%d", teamId, stationName, packetType, length)
		}
	}
}

// encodeStatusPacket builds a minimal UDP status packet with DS/Radio/Rio/Robot
// linked flags set so Cheesy Arena treats the robot as connected and ready.
func encodeStatusPacket(seq uint16, teamId int, battery float64) []byte {
	pkt := make([]byte, 8)
	binary.BigEndian.PutUint16(pkt[0:2], seq)
	pkt[2] = 0                        // protocol version
	pkt[3] = 0x08 | 0x10 | 0x20       // RioLinked | RadioLinked | RobotLinked
	pkt[4] = byte(teamId >> 8 & 0xff) // team hi (server reads teamId from bytes 4-5)
	pkt[5] = byte(teamId & 0xff)      // team lo
	volts := int(battery)
	frac := int((battery - float64(volts)) * 256)
	pkt[6] = byte(volts)
	pkt[7] = byte(frac)
	return pkt
}

func stationNameFromByte(b byte) string {
	switch b {
	case 0:
		return "R1"
	case 1:
		return "R2"
	case 2:
		return "R3"
	case 3:
		return "B1"
	case 4:
		return "B2"
	case 5:
		return "B3"
	}
	return fmt.Sprintf("?%d", b)
}

// runRenderer draws the full dashboard on a timer. Writes via ANSI cursor moves
// (home + clear) so the output looks static in place.
func runRenderer(stop <-chan struct{}) {
	// Hide cursor for cleaner redraws.
	fmt.Print("\x1b[?25l")
	t := time.NewTicker(tuiRedrawIntervalMs * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			render()
		}
	}
}

func render() {
	view.mu.Lock()
	// Snapshot to minimize time under lock.
	keys := make([]byte, 0, len(view.stations))
	for k := range view.stations {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	stations := make([]stationState, 0, len(keys))
	for _, k := range keys {
		stations = append(stations, *view.stations[k])
	}
	events := append([]string(nil), view.events...)
	view.mu.Unlock()

	var buf strings.Builder
	// Home cursor + clear screen below.
	buf.WriteString("\x1b[H\x1b[J")
	buf.WriteString("Cheesy Arena Mock Driver Stations\n")
	buf.WriteString(strings.Repeat("-", 100) + "\n")
	buf.WriteString(fmt.Sprintf(
		"%-4s %-6s %-6s %-8s %-9s %-5s %-5s %-10s %-6s %-4s %-4s\n",
		"St", "Team", "TCP", "Mode", "State", "EStop", "AStop", "Match", "Rem", "UDP", "KA",
	))
	buf.WriteString(strings.Repeat("-", 100) + "\n")
	if len(stations) == 0 {
		buf.WriteString("  (waiting for station registrations...)\n")
	}
	for _, s := range stations {
		tcp := "down"
		if s.TcpUp {
			tcp = "UP"
		}
		mode := "teleop"
		if s.Auto {
			mode = "auto"
		}
		state := "disabled"
		if s.Enabled {
			state = "ENABLED"
		}
		match := fmt.Sprintf("%s#%d", matchTypeName(s.MatchType), s.MatchNum)
		buf.WriteString(fmt.Sprintf(
			"%-4s %-6d %-6s %-8s %-9s %-5v %-5v %-10s %-6d %-4d %-4d\n",
			s.Station, s.Team, tcp, mode, state, s.EStop, s.AStop, match, s.Remaining, s.UdpSent, s.KeepSent,
		))
		if s.LastGameData != "" {
			buf.WriteString(fmt.Sprintf("     game data: %q\n", s.LastGameData))
		}
	}
	buf.WriteString("\nRecent events:\n")
	if len(events) == 0 {
		buf.WriteString("  (none yet)\n")
	}
	for _, ev := range events {
		buf.WriteString("  " + ev + "\n")
	}
	buf.WriteString("\nCtrl-C to quit\n")
	fmt.Print(buf.String())
}
