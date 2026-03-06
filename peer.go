package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/mascarenhasmelson/wintun-tunnel/winipcfg"
	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wintun"
)

const (
	STUN_SERVER = "192.168.30.6"
	STUN_PORT   = 2020
	PUB_KEY     = 43
	TUN_NAME    = "melson"
	TUN_LOCAL   = "100.64.1.1" //cgnat IPs rfc 6598
	TUN_REMOTE  = "100.64.1.2"
	TUN_MTU     = 1400
	SRC_PORT    = 9999
)

type StunMsg struct {
	Host   uint32
	Port   uint16
	pubkey uint16
}

type ProbePkt struct {
	Magic  uint16
	pubkey uint16
	Seq    uint32
}

const (
	STUN_PING_INTERVAL_MS    = 500
	HOLEPUNCH_ROUNDS         = 4
	HOLEPUNCH_BURST          = 20
	HOLEPUNCH_BURST_DELAY_MS = 30
	HOLEPUNCH_ROUND_DELAY_MS = 150
	KEEPALIVE_INTERVAL_S     = 2
	PUNCH_WAIT_TIMEOUT_S     = 15
	TUNNEL_DEAD_TIMEOUT_S    = 5
	PROBE_MAGIC              = 0x4E50
)

type TunnelState int32

const (
	STATE_PUNCH_WAIT TunnelState = iota
	STATE_ALIVE
)

func (s TunnelState) String() string {
	switch s {
	case STATE_PUNCH_WAIT:
		return "PUNCH_WAIT"
	case STATE_ALIVE:
		return "ALIVE"
	default:
		return "UNKNOWN"
	}
}

func logts(level, format string, args ...interface{}) {
	now := time.Now()
	timestamp := now.Format("15:04:05")
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("[%s][%s] %s\n", timestamp, level, msg)
}

func LOG_INFO(format string, args ...interface{})  { logts("INFO ", format, args...) }
func LOG_WARN(format string, args ...interface{})  { logts("WARN ", format, args...) }
func LOG_ERROR(format string, args ...interface{}) { logts("ERROR", format, args...) }
func LOG_DEBUG(format string, args ...interface{}) { logts("DEBUG", format, args...) }
func LOG_PUNCH(format string, args ...interface{}) { logts("PUNCH", format, args...) }
func LOG_TUN(format string, args ...interface{})   { logts("TUN  ", format, args...) }
func LOG_KEEP(format string, args ...interface{})  { logts("ALIVE", format, args...) }
func LOG_STATE(format string, args ...interface{}) { logts("STATE", format, args...) }
func LOG_TIME(format string, args ...interface{})  { logts("TIMER", format, args...) }

type udpPacket struct {
	data []byte
}
type NATClient struct {
	stunAddr    *net.UDPAddr
	sock        *net.UDPConn
	adapter     *wintun.Adapter
	session     *wintun.Session
	readWait    windows.Handle
	tunnelReady bool

	restarts   uint64
	pktsSent   uint64
	pktsRecv   uint64
	kaSent     uint64
	probesRecv uint64
}

func main() {
	LOG_INFO("STUN server  : %s:%d", STUN_SERVER, STUN_PORT)
	LOG_INFO("Session ID   : %d", PUB_KEY)
	LOG_INFO("Tunnel name  : %s", TUN_NAME)
	LOG_INFO("Local IP     : %s", TUN_LOCAL)
	LOG_INFO("Remote IP    : %s", TUN_REMOTE)
	LOG_INFO("MTU          : %d", TUN_MTU)
	LOG_INFO("Source port  : %d", SRC_PORT)

	client, err := NewNATClient()
	if err != nil {
		LOG_ERROR("Failed to create client: %v", err)
		os.Exit(1)
	}
	defer client.Close()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go client.Run()

	LOG_INFO("Client running. Press Ctrl+C to exit...")
	<-sigChan
	LOG_INFO("Shutting down...")
}

func NewNATClient() (*NATClient, error) {
	stunAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", STUN_SERVER, STUN_PORT))
	if err != nil {
		return nil, fmt.Errorf("resolve STUN server: %w", err)
	}
	LOG_INFO("STUN: %s -> %s", STUN_SERVER, stunAddr)

	localAddr := &net.UDPAddr{IP: net.IPv4zero, Port: SRC_PORT}
	sock, err := net.ListenUDP("udp", localAddr)
	if err != nil {
		return nil, fmt.Errorf("bind UDP: %w", err)
	}
	sock.SetDeadline(time.Time{})
	LOG_INFO("Bound to UDP port: %d", SRC_PORT)
	client := &NATClient{stunAddr: stunAddr, sock: sock}
	if err := client.createTunnel(); err != nil {
		sock.Close()
		return nil, fmt.Errorf("TUN setup failed: %w", err)
	}
	return client, nil
}

func (c *NATClient) createTunnel() error {
	LOG_INFO("Creating TUN interface...")

	guid := windows.GUID{
		Data1: 0xdeadbabe,
		Data2: 0xcafe,
		Data3: 0xbeef,
		Data4: [8]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef},
	}
	adapter, err := wintun.CreateAdapter(TUN_NAME, "Test", &guid)
	if err != nil {
		return fmt.Errorf("create adapter: %w", err)
	}
	c.adapter = adapter
	LOG_INFO("Adapter created, starting session...")

	session, err := adapter.StartSession(0x400000)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	c.session = &session
	c.readWait = session.ReadWaitEvent()
	LOG_INFO("Session started, configuring interface...")

	if err := c.configureTunnel(); err != nil {
		return err
	}

	LOG_INFO("Waiting for interface to be ready...")
	time.Sleep(2 * time.Second)

	if err := c.verifyInterface(); err != nil {
		LOG_WARN("Interface verification failed: %v", err)
	}

	c.tunnelReady = true
	LOG_INFO("TUN '%s'  local=%s  remote=%s  mtu=%d ✓ READY", TUN_NAME, TUN_LOCAL, TUN_REMOTE, TUN_MTU)
	return nil
}

func (c *NATClient) configureTunnel() error {
	time.Sleep(1 * time.Second)
	cmd := exec.Command("netsh", "interface", "ip", "set", "address",
		TUN_NAME, "static", TUN_LOCAL, "255.255.255.0")
	if out, err := cmd.CombinedOutput(); err != nil {
		LOG_ERROR("Set IP failed: %v, output: %s", err, string(out))
		return fmt.Errorf("set IP address: %w", err)
	}
	LOG_INFO("IP address set: %s", TUN_LOCAL)
	cmd = exec.Command("netsh", "interface", "ipv4", "set", "subinterface",
		TUN_NAME, fmt.Sprintf("mtu=%d", TUN_MTU), "store=persistent")
	if err := cmd.Run(); err != nil {
		LOG_WARN("Failed to set MTU: %v", err)
	} else {
		LOG_INFO("MTU set: %d", TUN_MTU)
	}
	cmd = exec.Command("route", "add", TUN_REMOTE, "mask", "255.255.255.255",
		TUN_LOCAL, "metric", "1")
	if err := cmd.Run(); err != nil {
		LOG_WARN("Failed to add route: %v", err)
	} else {
		LOG_INFO("Route added: %s via %s", TUN_REMOTE, TUN_LOCAL)
	}
	return nil
}

func (c *NATClient) verifyInterface() error {
	guid := c.adapter.LUID()
	luid := winipcfg.LUID(guid)
	iface, err := luid.Interface()
	if err != nil {
		return fmt.Errorf("get interface: %w", err)
	}
	if iface.OperStatus != winipcfg.IfOperStatusUp {
		return fmt.Errorf("interface not up: status=%d", iface.OperStatus)
	}
	LOG_INFO("Interface status: UP")
	return nil
}

func (c *NATClient) Run() {
	for !c.tunnelReady {
		LOG_INFO("Waiting for tunnel to be ready...")
		time.Sleep(500 * time.Millisecond)
	}
	for {
		c.restarts++
		remoteAddr := c.stunPhase()
		if remoteAddr == nil {
			continue
		}
		LOG_STATE("PAIRED  peer=%s  (session=%d)", remoteAddr, PUB_KEY)
		if remoteAddr.IP.Equal(c.stunAddr.IP) {
			LOG_WARN("Peer IP == STUN IP — possible hairpin NAT!")
		}
		c.holepunchBurst(remoteAddr)
		c.tunnelLoop(remoteAddr)
	}
}

func (c *NATClient) stunPhase() *net.UDPAddr {
	LOG_STATE("STUN  restart=#%d  session=%d", c.restarts, PUB_KEY)
	buf := make([]byte, 8)
	binary.BigEndian.PutUint32(buf[0:4], 0)
	binary.BigEndian.PutUint16(buf[4:6], 0)
	binary.BigEndian.PutUint16(buf[6:8], PUB_KEY)
	pings := 0
	for {
		c.sock.WriteToUDP(buf, c.stunAddr)
		pings++
		if pings%10 == 0 {
			LOG_INFO("Waiting for peer... (%d pings)", pings)
		}
		c.sock.SetReadDeadline(time.Now().Add(STUN_PING_INTERVAL_MS * time.Millisecond))
		replyBuf := make([]byte, 8)
		n, addr, err := c.sock.ReadFromUDP(replyBuf)
		if err == nil && n == 8 && addr.IP.Equal(c.stunAddr.IP) && addr.Port == c.stunAddr.Port {
			hostBE := binary.BigEndian.Uint32(replyBuf[0:4])
			portBE := binary.BigEndian.Uint16(replyBuf[4:6])
			sid := binary.BigEndian.Uint16(replyBuf[6:8])
			if sid == PUB_KEY && hostBE != 0 {
				ip := net.IPv4(byte(hostBE>>24), byte(hostBE>>16), byte(hostBE>>8), byte(hostBE))
				remoteAddr := &net.UDPAddr{IP: ip, Port: int(portBE)}
				LOG_INFO("STUN reply: peer=%s (raw host=0x%08x port=%d)", remoteAddr, hostBE, int(portBE))
				return remoteAddr
			}
		}
	}
}

func (c *NATClient) holepunchBurst(peer *net.UDPAddr) {
	LOG_PUNCH("Hole punch -> %s  (%d rounds x %d probes)", peer, HOLEPUNCH_ROUNDS, HOLEPUNCH_BURST)
	buf := make([]byte, 8)
	for r := 0; r < HOLEPUNCH_ROUNDS; r++ {
		for i := 0; i < HOLEPUNCH_BURST; i++ {
			binary.BigEndian.PutUint16(buf[0:2], PROBE_MAGIC)
			binary.BigEndian.PutUint16(buf[2:4], PUB_KEY)
			binary.BigEndian.PutUint32(buf[4:8], uint32(r*HOLEPUNCH_BURST+i))
			c.sock.WriteToUDP(buf, peer)
			time.Sleep(HOLEPUNCH_BURST_DELAY_MS * time.Millisecond)
		}
		LOG_PUNCH("  Round %d/%d done", r+1, HOLEPUNCH_ROUNDS)
		time.Sleep(HOLEPUNCH_ROUND_DELAY_MS * time.Millisecond)
	}
	LOG_PUNCH("Burst done. Waiting up to %ds for peer...", PUNCH_WAIT_TIMEOUT_S)
}

func (c *NATClient) tunnelLoop(remoteAddr *net.UDPAddr) {
	var stopped int32
	stop := func() bool { return atomic.LoadInt32(&stopped) == 1 }
	type udpMsg struct {
		data []byte
		addr *net.UDPAddr
	}
	udpCh := make(chan udpMsg, 256)
	tunCh := make(chan []byte, 256)
	c.sock.SetDeadline(time.Time{})
	go func() {
		buf := make([]byte, 4096)
		for !stop() {
			n, addr, err := c.sock.ReadFromUDP(buf)
			if err != nil {
				if !stop() {
					LOG_WARN("UDP read error: %v", err)
				}
				return
			}
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				udpCh <- udpMsg{data: data, addr: addr}
			}
		}
	}()

	go func() {
		for !stop() {
			packet, err := c.session.ReceivePacket()
			if err == windows.ERROR_NO_MORE_ITEMS {
				windows.WaitForSingleObject(c.readWait, 100)
				continue
			}
			if err == windows.ERROR_HANDLE_EOF {
				return
			}
			if err != nil {
				if !stop() {
					LOG_WARN("TUN ReceivePacket error: %v", err)
				}
				return
			}
			data := make([]byte, len(packet))
			pktData := unsafe.Slice((*byte)(unsafe.Pointer(&packet[0])), len(packet))
			copy(data, pktData)
			c.session.ReleaseReceivePacket(packet)
			tunCh <- data
		}
	}()
	now := time.Now()
	lastSent := now
	var lastRecv time.Time
	tstart := now
	state := STATE_PUNCH_WAIT
	lastCountdown := time.Time{}

	LOG_STATE("PUNCH_WAIT  peer=%s  (timeout in %ds)", remoteAddr, PUNCH_WAIT_TIMEOUT_S)

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	defer func() {
		atomic.StoreInt32(&stopped, 1)
		c.sock.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
	}()

	for {
		select {
		case <-ticker.C:
			now = time.Now()

		case msg := <-udpCh:
			now = time.Now()
			addr := msg.addr
			data := msg.data
			n := len(data)

			if addr.IP.Equal(remoteAddr.IP) && addr.Port == remoteAddr.Port {
				lastRecv = now

				if n == 1 && data[0] == 0x00 {
					if state == STATE_PUNCH_WAIT {
						state = STATE_ALIVE
						LOG_STATE("ALIVE  keepalive from %s  (punch took %s)",
							addr, now.Sub(tstart).Truncate(time.Second))
					} else {
						LOG_KEEP("recv  peer=%s", addr)
					}
				} else if n == 8 && binary.BigEndian.Uint16(data[0:2]) == PROBE_MAGIC {
					c.probesRecv++
					if state == STATE_PUNCH_WAIT {
						state = STATE_ALIVE
						seq := binary.BigEndian.Uint32(data[4:8])
						LOG_STATE("ALIVE  probe #%d from %s", seq, addr)
					}
				} else {
					if state == STATE_PUNCH_WAIT {
						state = STATE_ALIVE
						LOG_STATE("ALIVE  first data from %s", addr)
					}
					if c.tunnelReady {
						sendPacket, err := c.session.AllocateSendPacket(n)
						if err == nil {
							dst := unsafe.Slice((*byte)(unsafe.Pointer(&sendPacket[0])), n)
							copy(dst, data)
							c.session.SendPacket(sendPacket)
							c.pktsRecv++
							LOG_TUN("UDP->TUN  %d bytes", n)
						} else if err != windows.ERROR_BUFFER_OVERFLOW {
							LOG_WARN("AllocateSendPacket: %v", err)
						}
					}
				}
			} else if addr.IP.Equal(c.stunAddr.IP) && addr.Port == c.stunAddr.Port {
				LOG_DEBUG("Late STUN packet — ignored")
			} else {
				LOG_WARN("Unknown UDP from %s — ignored", addr)
			}
			continue

		case data := <-tunCh:
			now = time.Now()
			LOG_TUN("TUN->UDP  %d bytes", len(data))
			if _, err := c.sock.WriteToUDP(data, remoteAddr); err == nil {
				lastSent = now
				c.pktsSent++
			} else {
				LOG_WARN("sendto: %v", err)
			}
			continue
		}
		if now.Sub(lastSent) >= KEEPALIVE_INTERVAL_S*time.Second {
			if _, err := c.sock.WriteToUDP([]byte{0x00}, remoteAddr); err == nil {
				lastSent = now
				c.kaSent++
				LOG_KEEP("sent -> %s  (state=%s  total=%d)", remoteAddr, state, c.kaSent)
			}
		}
		if now.Unix() != lastCountdown.Unix() {
			lastCountdown = now
			if state == STATE_PUNCH_WAIT {
				elapsed := now.Sub(tstart)
				remaining := time.Duration(PUNCH_WAIT_TIMEOUT_S)*time.Second - elapsed
				if remaining > 0 {
					LOG_TIME("PUNCH_WAIT  %s elapsed  %s until STUN restart",
						elapsed.Truncate(time.Second), remaining.Truncate(time.Second))
				}
			} else if !lastRecv.IsZero() {
				silence := now.Sub(lastRecv)
				remaining := time.Duration(TUNNEL_DEAD_TIMEOUT_S)*time.Second - silence
				if silence >= 2*time.Second && remaining > 0 {
					LOG_TIME("ALIVE  %s silence  %s until restart",
						silence.Truncate(time.Second), remaining.Truncate(time.Second))
				}
			}
		}

		if state == STATE_PUNCH_WAIT && now.Sub(tstart) >= PUNCH_WAIT_TIMEOUT_S*time.Second {
			LOG_STATE("PUNCH_WAIT TIMEOUT — back to STUN")
			return
		}
		if state == STATE_ALIVE && !lastRecv.IsZero() &&
			now.Sub(lastRecv) >= TUNNEL_DEAD_TIMEOUT_S*time.Second {
			LOG_STATE("TUNNEL DEAD  silence=%s — back to STUN",
				now.Sub(lastRecv).Truncate(time.Second))
			LOG_STATE("  sent=%d recv=%d probes=%d restarts=%d",
				c.pktsSent, c.pktsRecv, c.probesRecv, c.restarts)
			return
		}
	}
}

func (c *NATClient) Close() {
	c.tunnelReady = false
	if c.sock != nil {
		c.sock.Close()
	}
	if c.session != nil {
		c.session.End()
	}
	if c.adapter != nil {
		c.adapter.Close()
	}
	LOG_INFO("Client closed")
}
