package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestParseProxyValid(t *testing.T) {
	p, err := parseProxy("socks5://127.0.0.1:9050")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Scheme != "socks5" || p.Host != "127.0.0.1" || p.Port != 9050 {
		t.Fatalf("unexpected proxy: %#v", p)
	}
}

func TestParseProxyRejectsBadPort(t *testing.T) {
	if _, err := parseProxy("socks5://127.0.0.1:70000"); err == nil {
		t.Fatal("expected invalid port error")
	}
}

func TestPACRoutesOnlyGatewayToRelay(t *testing.T) {
	pac := makePAC("127.0.0.1", 17777)
	if !containsAll(pac, []string{"gateway.discord.gg", "PROXY 127.0.0.1:17777", "DIRECT"}) {
		t.Fatalf("unexpected PAC: %s", pac)
	}
}

func TestSelectProxyExcludesCountries(t *testing.T) {
	entries := []ProxyEntry{
		{Proxy: "socks5://1.1.1.1:1080", CountryCode: "BR"},
		{Proxy: "socks5://2.2.2.2:1080", CountryCode: "US"},
	}
	got := filterEntries(entries, map[string]bool{"BR": true})
	if len(got) != 1 || got[0].CountryCode != "US" {
		t.Fatalf("unexpected filtered entries: %#v", got)
	}
}

func containsAll(s string, needles []string) bool {
	for _, n := range needles {
		if !contains(s, n) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestBuildDiscordArgsIncludesPACAndRemoteDebugging(t *testing.T) {
	args := buildDiscordArgs("http://127.0.0.1:12345/proxy.pac", 9229)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--proxy-pac-url=http://127.0.0.1:12345/proxy.pac") {
		t.Fatalf("PAC arg missing: %q", joined)
	}
	if !strings.Contains(joined, "--remote-debugging-port=9229") {
		t.Fatalf("debug arg missing: %q", joined)
	}
}

func TestWatcherScriptHooksVoiceAndSignalsOnlyAfterCapabilities(t *testing.T) {
	s := watcherScript("goliveSignal")
	for _, needle := range []string{
		"VOICE_STATE_UPDATE",
		"VOICE_STATE_UPDATES",
		"supportsInApp",
		"DESKTOP_CAPTURE",
		"VIDEO",
		"goliveSignal",
		"video-enabled",
	} {
		if !strings.Contains(s, needle) {
			t.Fatalf("watcher script missing %q", needle)
		}
	}
}

func TestChooseDiscordTargetPrefersWebpackPage(t *testing.T) {
	targets := []cdpTarget{
		{Type: "page", Title: "Updater", URL: "file:///updater.html", WebSocketDebuggerURL: "ws://one"},
		{Type: "page", Title: "Discord", URL: "https://discord.com/channels/@me", WebSocketDebuggerURL: "ws://two"},
	}
	idx := chooseDiscordTarget(targets)
	if idx != 1 {
		t.Fatalf("expected Discord target index 1, got %d", idx)
	}
}

func TestWSClientHandshakeAndTextFrame(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		line, err := br.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "GET ") {
			done <- fmt.Errorf("bad request line: %q %v", line, err)
			return
		}
		key := ""
		for {
			line, err = br.ReadString('\n')
			if err != nil {
				done <- err
				return
			}
			if line == "\r\n" {
				break
			}
			if strings.HasPrefix(strings.ToLower(line), "sec-websocket-key:") {
				key = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
			}
		}
		h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		accept := base64.StdEncoding.EncodeToString(h[:])
		fmt.Fprintf(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)

		// Read one masked client text frame.
		hdr := make([]byte, 2)
		if _, err := io.ReadFull(br, hdr); err != nil {
			done <- err
			return
		}
		if hdr[0]&0x0f != 1 || hdr[1]&0x80 == 0 {
			done <- fmt.Errorf("unexpected frame header: %v", hdr)
			return
		}
		n := int(hdr[1] & 0x7f)
		mask := make([]byte, 4)
		if _, err := io.ReadFull(br, mask); err != nil {
			done <- err
			return
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(br, payload); err != nil {
			done <- err
			return
		}
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
		if string(payload) != "hello" {
			done <- fmt.Errorf("unexpected payload %q", payload)
			return
		}

		// Send an unmasked server text frame.
		_, err = c.Write(append([]byte{0x81, 0x05}, []byte("world")...))
		done <- err
	}()

	ws, err := dialWS("ws://"+ln.Addr().String()+"/devtools/page/test", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if err := ws.WriteText("hello"); err != nil {
		t.Fatal(err)
	}
	got, err := ws.ReadText()
	if err != nil {
		t.Fatal(err)
	}
	if got != "world" {
		t.Fatalf("got %q", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
