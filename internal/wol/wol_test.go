package wol

import (
	"bytes"
	"net"
	"testing"
	"time"
)

// func Packet(mac string) ([]byte, error) sends 102 bytes: six 0xff bytes
// followed by the MAC repeated 16 times.
func TestPacketBuild(t *testing.T) {
	p, err := Packet("e8:ff:1e:d8:a1:15")
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 6+16*6 {
		t.Fatalf("len=%d, want 102", len(p))
	}
	for i := 0; i < 6; i++ {
		if p[i] != 0xff {
			t.Fatalf("byte %d = %#x, want 0xff", i, p[i])
		}
	}
	for i := 0; i < 16; i++ {
		seq := p[6+6*i : 6+6*i+6]
		if !bytes.Equal(seq, []byte{0xe8, 0xff, 0x1e, 0xd8, 0xa1, 0x15}) {
			t.Fatalf("copy %d = %x", i, seq)
		}
	}
	if _, err := Packet("not-a-mac"); err == nil {
		t.Fatal("want error for bad MAC")
	}
}

// Burst sends n datagrams per MAC to a real UDP socket; the listener
// verifies each one is a valid magic packet for that MAC.
func TestBurstDeliversRealUDP(t *testing.T) {
	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	done := make(chan int, 1)
	go func() {
		n := 0
		buf := make([]byte, 2048)
		deadline := time.Now().Add(5 * time.Second)
		ln.SetReadDeadline(deadline)
		for request := 0; request < 4; request++ {
			length, _, err := ln.ReadFrom(buf)
			if err != nil {
				break
			}
			p, err := Packet("e8:ff:1e:d8:a0:f4")
			if err != nil {
				t.Errorf("packet: %v", err)
				break
			}
			if !bytes.Equal(buf[:length], p) {
				t.Errorf("datagram %d mismatch: got %x", request, buf[:length])
			}
			n++
		}
		done <- n
	}()

	if err := Burst(ln.LocalAddr().String(), []string{"e8:ff:1e:d8:a0:f4"}, 4, 2*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got != 4 {
			t.Fatalf("listener saw %d datagrams, want 4", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listener timed out")
	}
}
