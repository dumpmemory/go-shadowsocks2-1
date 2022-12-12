package main

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Potterli20/go-shadowsocks2/socks"
)

type mode int

const (
	remoteServer mode = iota
	relayClient
	socksClient
)

const udpBufSize = 64 * 1024

var bufPool = sync.Pool{New: func() any { return make([]byte, udpBufSize) }}

// Listen on laddr for UDP packets, encrypt and send to server to reach target.
func udpLocal(laddr, server, target string, shadow func(net.PacketConn) net.PacketConn) {
	srvAddr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		logf("UDP server address error: %v", err)
		return
	}

	tgt := socks.ParseAddr(target)
	if tgt == nil {
		err = fmt.Errorf("invalid target address: %q", target)
		logf("UDP target address error: %v", err)
		return
	}

	c, err := net.ListenPacket("udp", laddr)
	if err != nil {
		logf("UDP local listen error: %v", err)
		return
	}
	defer c.Close()

	m := make(map[string]chan []byte)
	var lock sync.Mutex

	logf("UDP tunnel %s <-> %s <-> %s", laddr, server, target)
	for {
		buf := bufPool.Get().([]byte)
		copy(buf, tgt)
		n, raddr, err := c.ReadFrom(buf[len(tgt):])
		if err != nil {
			logf("UDP local read error: %v", err)
			continue
		}

		lock.Lock()
		k := raddr.String()
		ch := m[k]
		if ch == nil {
			pc, err := net.ListenPacket("udp", "")
			if err != nil {
				logf("failed to create UDP socket: %v", err)
				goto Unlock
			}
			pc = shadow(pc)
			ch = make(chan []byte, 1) // must use buffered chan
			m[k] = ch

			go func() { // recv from user and send to udpRemote
				for buf := range ch {
					pc.SetReadDeadline(time.Now().Add(config.UDPTimeout)) // extend read timeout
					if _, err := pc.WriteTo(buf, srvAddr); err != nil {
						logf("UDP local write error: %v", err)
					}
					bufPool.Put(buf[:cap(buf)])
				}
			}()

			go func() { // recv from udpRemote and send to user
				if err := timedCopy(raddr, c, pc, config.UDPTimeout, false); err != nil {
					if err, ok := err.(net.Error); ok && err.Timeout() {
						// ignore i/o timeout
					} else {
						logf("timedCopy error: %v", err)
					}
				}
				pc.Close()
				lock.Lock()
				if ch := m[k]; ch != nil {
					close(ch)
				}
				delete(m, k)
				lock.Unlock()
			}()
		}
	Unlock:
		lock.Unlock()

		select {
		case ch <- buf[:len(tgt)+n]: // send
		default: // drop
			bufPool.Put(buf)
		}
	}
}

// Listen on laddr for Socks5 UDP packets, encrypt and send to server to reach target.
func udpSocksLocal(laddr, server string, shadow func(net.PacketConn) net.PacketConn) {
	srvAddr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		logf("UDP server address error: %v", err)
		return
	}

	c, err := net.ListenPacket("udp", laddr)
	if err != nil {
		logf("UDP local listen error: %v", err)
		return
	}
	defer c.Close()

	nm := newNATmap(config.UDPTimeout)
	buf := make([]byte, udpBufSize)

	for {
		n, raddr, err := c.ReadFrom(buf)
		if err != nil {
			logf("UDP local read error: %v", err)
			continue
		}

		pc := nm.Get(raddr.String())
		if pc == nil {
			pc, err = net.ListenPacket("udp", "")
			if err != nil {
				logf("UDP local listen error: %v", err)
				continue
			}
			logf("UDP socks tunnel %s <-> %s <-> %s", laddr, server, socks.Addr(buf[3:]))
			pc = shadow(pc)
			nm.Add(raddr, c, pc, socksClient)
		}

		_, err = pc.WriteTo(buf[3:n], srvAddr)
		if err != nil {
			logf("UDP local write error: %v", err)
			continue
		}
	}
}

// Listen on addr for encrypted packets and basically do UDP NAT.
func udpRemote(addr string, shadow func(net.PacketConn) net.PacketConn) {
	c, err := net.ListenPacket("udp", addr)
	if err != nil {
		logf("UDP remote listen error: %v", err)
		return
	}
	defer c.Close()
	c = shadow(c)

	m := make(map[string]chan []byte)
	var lock sync.Mutex

	logf("listening UDP on %s", addr)
	for {
		buf := bufPool.Get().([]byte)
		n, raddr, err := c.ReadFrom(buf)
		if err != nil {
			logf("UDP remote read error: %v", err)
			continue
		}

		lock.Lock()
		k := raddr.String()
		ch := m[k]
		if ch == nil {
			pc, err := net.ListenPacket("udp", "")
			if err != nil {
				logf("failed to create UDP socket: %v", err)
				goto Unlock
			}
			ch = make(chan []byte, 1) // must use buffered chan
			m[k] = ch

			go func() { // receive from udpLocal and send to target
				var tgtUDPAddr *net.UDPAddr
				var err error

				for buf := range ch {
					tgtAddr := socks.SplitAddr(buf)
					if tgtAddr == nil {
						logf("failed to split target address from packet: %q", buf)
						goto End
					}
					tgtUDPAddr, err = net.ResolveUDPAddr("udp", tgtAddr.String())
					if err != nil {
						logf("failed to resolve target UDP address: %v", err)
						goto End
					}
					pc.SetReadDeadline(time.Now().Add(config.UDPTimeout))
					if _, err = pc.WriteTo(buf[len(tgtAddr):], tgtUDPAddr); err != nil {
						logf("UDP remote write error: %v", err)
						goto End
					}
				End:
					bufPool.Put(buf[:cap(buf)])
				}
			}()

			go func() { // receive from udpLocal and send to client
				if err := timedCopy(raddr, c, pc, config.UDPTimeout, true); err != nil {
					if err, ok := err.(net.Error); ok && err.Timeout() {
						// ignore i/o timeout
					} else {
						logf("timedCopy error: %v", err)
					}
				}
				pc.Close()
				lock.Lock()
				if ch := m[k]; ch != nil {
					close(ch)
				}
				delete(m, k)
				lock.Unlock()
			}()
		}
	Unlock:
		lock.Unlock()

		select {
		case ch <- buf[:n]: // sent
		default: // drop
			bufPool.Put(buf)
		}
	}
}

// Packet NAT table
type natmap struct {
	sync.RWMutex
	m       map[string]net.PacketConn
	timeout time.Duration
}

func newNATmap(timeout time.Duration) *natmap {
	m := &natmap{}
	m.m = make(map[string]net.PacketConn)
	m.timeout = timeout
	return m
}

func (m *natmap) Get(key string) net.PacketConn {
	m.RLock()
	defer m.RUnlock()
	return m.m[key]
}

func (m *natmap) Set(key string, pc net.PacketConn) {
	m.Lock()
	defer m.Unlock()

	m.m[key] = pc
}

func (m *natmap) Del(key string) net.PacketConn {
	m.Lock()
	defer m.Unlock()

	pc, ok := m.m[key]
	if ok {
		delete(m.m, key)
		return pc
	}
	return nil
}

func (m *natmap) Add(peer net.Addr, dst, src net.PacketConn, role mode) {
	m.Set(peer.String(), src)

	go func() {
		timedCopy(dst, peer, src, m.timeout, role)
		if pc := m.Del(peer.String()); pc != nil {
			pc.Close()
		}
	}()
}

// copy from src to dst at target with read timeout
func timedCopy(dst net.PacketConn, target net.Addr, src net.PacketConn, timeout time.Duration, role mode) error {
	buf := make([]byte, udpBufSize)

	for {
		src.SetReadDeadline(time.Now().Add(timeout))
		n, raddr, err := src.ReadFrom(buf)
		if err != nil {
			return err
		}

		switch role {
		case remoteServer: // server -> client: add original packet source
			srcAddr := socks.ParseAddr(raddr.String())
			copy(buf[len(srcAddr):], buf[:n])
			copy(buf, srcAddr)
			_, err = dst.WriteTo(buf[:len(srcAddr)+n], target)
		case relayClient: // client -> user: strip original packet source
			srcAddr := socks.SplitAddr(buf[:n])
			_, err = dst.WriteTo(buf[len(srcAddr):n], target)
		case socksClient: // client -> socks5 program: just set RSV and FRAG = 0
			_, err = dst.WriteTo(append([]byte{0, 0, 0}, buf[:n]...), target)
		}

		if err != nil {
			return err
		}
	}
}
