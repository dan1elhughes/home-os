package wol

import (
	"fmt"
	"net"
	"time"
)

// Packet builds the magic packet for one MAC: six 0xff bytes followed by
// the MAC repeated 16 times.
func Packet(mac string) ([]byte, error) {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return nil, fmt.Errorf("magic packet: %w", err)
	}
	if len(hw) != 6 {
		return nil, fmt.Errorf("magic packet: %s: not a 6-byte MAC", mac)
	}
	p := make([]byte, 6+16*6)
	for i := range p[:6] {
		p[i] = 0xff
	}
	for i := 0; i < 16; i++ {
		copy(p[6+6*i:], hw)
	}
	return p, nil
}

// Burst sends n magic-packet datagrams to bcastAddr (host:port, normally
// the LAN broadcast on port 9) for each MAC, gap apart, best effort per
// MAC.
func Burst(bcastAddr string, macs []string, n int, gap time.Duration) error {
	var allErr error
	for _, mac := range macs {
		p, err := Packet(mac)
		if err != nil {
			return err
		}
		conn, err := net.ListenPacket("udp", ":0")
		if err != nil {
			return err
		}
		ra, err := net.ResolveUDPAddr("udp", bcastAddr)
		if err != nil {
			conn.Close()
			return err
		}
		var lastErr error
		for i := 0; i < n; i++ {
			if _, err := conn.WriteTo(p, ra); err != nil {
				lastErr = err
				break
			}
			time.Sleep(gap)
		}
		conn.Close()
		if lastErr != nil {
			if allErr == nil {
				allErr = fmt.Errorf("%s: %w", mac, lastErr)
			}
		}
	}
	return allErr
}
