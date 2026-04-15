// Mock HP ProCurve 2810-series switch for integration testing of the
// cheesy-arena HPSwitch driver. Speaks just enough of the ProCurve telnet
// login/config flow to keep the client happy and log every command the
// arena sends.
//
// Run:
//
//	go run ./tools/mock-hp-switch -port 2323 -password password
//
// The HPSwitch client writes the entire session (banner-ack, password,
// commands, exit) in a single flush, then blocks on ReadFrom until the
// server closes. So the mock just needs to drain the input with an idle
// timeout, echo a plausible prompt/output, then close.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "0.0.0.0", "bind address")
	port := flag.Int("port", 2323, "TCP port (use 23 for the real ProCurve port; requires root/CAP_NET_BIND_SERVICE)")
	password := flag.String("password", "", "expected password (optional; empty = accept any)")
	idleMs := flag.Int("idle-ms", 400, "close connection after this many ms of input silence")
	flag.Parse()

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", *addr, *port))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("mock HP ProCurve listening on %s (password=%q)", ln.Addr(), *password)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go handle(conn, *password, time.Duration(*idleMs)*time.Millisecond)
	}
}

func handle(conn net.Conn, password string, idle time.Duration) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	log.Printf("[%s] === session open", remote)

	// Real switch prints banner then "Press any key to continue". The client
	// only cares that the TCP connection accepts writes, so we skip emitting
	// the banner and just read. Echo something once to mimic a prompt.
	fmt.Fprint(conn, "ProCurve Switch 2810-24G# ")

	buf := make([]byte, 4096)
	var session strings.Builder
	for {
		conn.SetReadDeadline(time.Now().Add(idle))
		n, err := conn.Read(buf)
		if n > 0 {
			session.Write(buf[:n])
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() && session.Len() > 0 {
				break // idle: client done writing
			}
			if err == io.EOF {
				break
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue // still waiting for first byte
			}
			log.Printf("[%s] read err: %v", remote, err)
			return
		}
	}

	raw := session.String()
	logSession(remote, raw, password)

	// Emit a minimal plausible reply so any future show-command clients see
	// something. Current HPSwitch only calls configure commands and discards
	// output, so this is mostly cosmetic.
	fmt.Fprint(conn, "\r\nOK\r\n")
	log.Printf("[%s] === session close", remote)
}

// logSession prints the commands the client sent, redacting the password
// line if it matches the expected one. The HPSwitch flow is:
//
//	\n                      (press-any-key ack)
//	<password>\n
//	no page\n
//	<command block>
//	exit\n
//
// or for config commands the block is wrapped in configure terminal ... exit / write memory.
func logSession(remote, raw, password string) {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	log.Printf("[%s] received %d bytes, %d lines:", remote, len(raw), len(lines))
	// HPSwitch sends: "" (press-any-key) then password then commands. The
	// password is always the second token in the line stream.
	pwIdx := -1
	for i, ln := range lines {
		if ln == "" {
			continue
		}
		pwIdx = i
		break
	}
	for i, ln := range lines {
		display := ln
		switch {
		case i < pwIdx && ln == "":
			display = "<press-any-key>"
		case i == pwIdx && password != "" && ln == password:
			display = "<password ok>"
		case i == pwIdx && password != "" && ln != password:
			display = fmt.Sprintf("<password MISMATCH: got %q want %q>", ln, password)
		}
		fmt.Fprintf(os.Stderr, "    %s\n", display)
	}
}
