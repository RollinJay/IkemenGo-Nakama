package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	p2pMagic             = "IKEMEN-P2P/1"
	p2pHello             = "hello"
	p2pAck               = "ack"
	p2pDefaultInterval   = 75 * time.Millisecond
	p2pDefaultTimeout    = 5 * time.Second
	stunBindingRequest   = 0x0001
	stunBindingSuccess   = 0x0101
	stunMappedAddress    = 0x0001
	stunXORMappedAddress = 0x0020
	stunMagicCookie      = 0x2112A442
)

type P2PCandidate struct {
	Type    string `json:"type"`
	Address string `json:"address"`
	Port    int    `json:"port"`
}

type P2PSignal struct {
	Magic      string         `json:"magic"`
	MatchID    string         `json:"match_id"`
	Token      string         `json:"token"`
	Candidates []P2PCandidate `json:"candidates"`
}

type p2PPacket struct {
	Magic   string `json:"magic"`
	Kind    string `json:"kind"`
	MatchID string `json:"match_id"`
	Token   string `json:"token"`
}

// NakamaP2P owns the UDP socket used for NAT discovery and hole punching.
// The socket deliberately remains open after a successful handshake so the
// caller can transfer it to the GGPO transport without losing the NAT mapping.
type NakamaP2P struct {
	mu          sync.Mutex
	conn        *net.UDPConn
	matchID     string
	token       string
	remoteToken string
	remoteAddr  *net.UDPAddr
	local       []P2PCandidate
	remote      []P2PCandidate
	ready       chan struct{}
	closed      chan struct{}
	readyOnce   sync.Once
	closeOnce   sync.Once
	signal      func(P2PSignal) error
	interval    time.Duration
	timeout     time.Duration
}

func NewNakamaP2P(matchID string, localPort int, signal func(P2PSignal) error) (*NakamaP2P, error) {
	if matchID == "" {
		return nil, errors.New("p2p match id is empty")
	}
	if localPort < 0 || localPort > 65535 {
		return nil, errors.New("p2p local port is invalid")
	}
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("generate p2p token: %w", err)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: localPort})
	if err != nil {
		return nil, fmt.Errorf("bind p2p UDP socket: %w", err)
	}
	p := &NakamaP2P{
		conn:     conn,
		matchID:  matchID,
		token:    hex.EncodeToString(tokenBytes),
		ready:    make(chan struct{}),
		closed:   make(chan struct{}),
		signal:   signal,
		interval: p2pDefaultInterval,
		timeout:  p2pDefaultTimeout,
	}
	return p, nil
}

func (p *NakamaP2P) UDPConn() *net.UDPConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn
}

func (p *NakamaP2P) LocalPort() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn == nil || p.conn.LocalAddr() == nil {
		return 0
	}
	return p.conn.LocalAddr().(*net.UDPAddr).Port
}

func (p *NakamaP2P) LocalCandidates() []P2PCandidate {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]P2PCandidate(nil), p.local...)
}

func (p *NakamaP2P) RemoteAddr() *net.UDPAddr {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.remoteAddr == nil {
		return nil
	}
	copy := *p.remoteAddr
	return &copy
}

func (p *NakamaP2P) Wait(ctx context.Context) error {
	select {
	case <-p.ready:
		return nil
	case <-p.closed:
		return errors.New("p2p connection closed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *NakamaP2P) Start(ctx context.Context, stunServers []string) error {
	local, err := p.gatherCandidates(ctx, stunServers)
	if err != nil {
		p.Close()
		return err
	}
	p.mu.Lock()
	p.local = local
	p.mu.Unlock()
	if p.signal != nil {
		if err := p.signal(P2PSignal{Magic: p2pMagic, MatchID: p.matchID, Token: p.token, Candidates: local}); err != nil {
			p.Close()
			return err
		}
	}
	go p.punchLoop()
	return nil
}

func (p *NakamaP2P) HandleSignal(signal P2PSignal) error {
	if signal.Magic != p2pMagic {
		return errors.New("invalid p2p signal magic")
	}
	if signal.MatchID != p.matchID {
		return errors.New("p2p signal belongs to another match")
	}
	if signal.Token == "" {
		return errors.New("p2p signal has no peer token")
	}
	p.mu.Lock()
	if signal.Token == p.token {
		p.mu.Unlock()
		// Nakama match broadcasts may be delivered to the sender as well.
		// Ignore the local signal instead of treating our own socket as the peer.
		return nil
	}
	p.remoteToken = signal.Token
	p.remote = append([]P2PCandidate(nil), signal.Candidates...)
	p.mu.Unlock()
	return nil
}

func (p *NakamaP2P) gatherCandidates(ctx context.Context, stunServers []string) ([]P2PCandidate, error) {
	p.mu.Lock()
	conn := p.conn
	p.mu.Unlock()
	if conn == nil {
		return nil, errors.New("p2p UDP socket is closed")
	}
	port := p.LocalPort()
	candidates := make([]P2PCandidate, 0, 8)
	addresses, err := localIPv4Addresses()
	if err != nil {
		return nil, err
	}
	for _, ip := range addresses {
		candidates = append(candidates, P2PCandidate{Type: "host", Address: ip.String(), Port: port})
	}
	for _, server := range stunServers {
		server = strings.TrimSpace(server)
		if server == "" {
			continue
		}
		addr, err := net.ResolveUDPAddr("udp4", server)
		if err != nil {
			log.Printf("P2P STUN resolve failed for %q: %v", server, err)
			continue
		}
		mapped, err := stunBinding(ctx, conn, addr)
		if err != nil {
			log.Printf("P2P STUN query failed for %q: %v", server, err)
			continue
		}
		candidates = append(candidates, P2PCandidate{Type: "srflx", Address: mapped.IP.String(), Port: mapped.Port})
		break
	}
	if len(candidates) == 0 {
		return nil, errors.New("no IPv4 P2P candidates were discovered")
	}
	return uniqueCandidates(candidates), nil
}

func (p *NakamaP2P) punchLoop() {
	timer := time.NewTicker(p.interval)
	defer timer.Stop()
	deadline := time.NewTimer(p.timeout)
	defer deadline.Stop()
	for {
		select {
		case <-p.closed:
			return
		case <-p.ready:
			return
		case <-deadline.C:
			p.Close()
			return
		case <-timer.C:
			p.sendPunches()
		}
	}
}

func (p *NakamaP2P) sendPunches() {
	p.mu.Lock()
	conn := p.conn
	remote := append([]P2PCandidate(nil), p.remote...)
	matchID := p.matchID
	token := p.token
	remoteToken := p.remoteToken
	p.mu.Unlock()
	if conn == nil || remoteToken == "" {
		return
	}
	payload, _ := json.Marshal(p2PPacket{Magic: p2pMagic, Kind: p2pHello, MatchID: matchID, Token: token})
	for _, candidate := range remote {
		addr := &net.UDPAddr{IP: net.ParseIP(candidate.Address), Port: candidate.Port}
		if addr.IP == nil || addr.Port <= 0 {
			continue
		}
		_, _ = conn.WriteToUDP(payload, addr)
	}
	_ = p.readHandshake(remoteToken)
}

func (p *NakamaP2P) readHandshake(expectedToken string) error {
	p.mu.Lock()
	conn := p.conn
	matchID := p.matchID
	p.mu.Unlock()
	if conn == nil {
		return errors.New("p2p UDP socket is closed")
	}
	_ = conn.SetReadDeadline(time.Now().Add(p.interval / 2))
	buf := make([]byte, 2048)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return nil
			}
			return err
		}
		var packet p2PPacket
		if json.Unmarshal(buf[:n], &packet) != nil || packet.Magic != p2pMagic || packet.MatchID != matchID {
			continue
		}
		if packet.Kind == p2pHello && packet.Token == expectedToken {
			ack, _ := json.Marshal(p2PPacket{Magic: p2pMagic, Kind: p2pAck, MatchID: matchID, Token: p.token})
			_, _ = conn.WriteToUDP(ack, addr)
			p.markReady(addr)
			_ = conn.SetReadDeadline(time.Time{})
			return nil
		}
		if packet.Kind == p2pAck && packet.Token == expectedToken {
			p.markReady(addr)
			_ = conn.SetReadDeadline(time.Time{})
			return nil
		}
	}
}

func (p *NakamaP2P) markReady(addr *net.UDPAddr) {
	p.mu.Lock()
	copy := *addr
	p.remoteAddr = &copy
	p.mu.Unlock()
	p.readyOnce.Do(func() { close(p.ready) })
}

// TakeUDPConn transfers ownership of the already-bound socket to the caller.
// The P2P object will no longer close the connection after the transfer.
func (p *NakamaP2P) TakeUDPConn() (*net.UDPConn, error) {
	if p == nil {
		return nil, errors.New("nil p2p connection")
	}
	select {
	case <-p.ready:
	default:
		return nil, errors.New("p2p connection is not ready")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn == nil {
		return nil, errors.New("p2p UDP socket is already closed or transferred")
	}
	conn := p.conn
	p.conn = nil
	return conn, nil
}

func (p *NakamaP2P) Close() {
	p.closeOnce.Do(func() {
		close(p.closed)
		p.mu.Lock()
		conn := p.conn
		p.conn = nil
		p.mu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
	})
}

func localIPv4Addresses() ([]net.IP, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []net.IP
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.To4() == nil {
				continue
			}
			ip = ip.To4()
			key := ip.String()
			if !seen[key] {
				seen[key] = true
				out = append(out, ip)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

func uniqueCandidates(in []P2PCandidate) []P2PCandidate {
	seen := make(map[string]bool)
	out := make([]P2PCandidate, 0, len(in))
	for _, c := range in {
		key := fmt.Sprintf("%s:%s:%d", c.Type, c.Address, c.Port)
		if !seen[key] {
			seen[key] = true
			out = append(out, c)
		}
	}
	return out
}

func stunBinding(ctx context.Context, conn *net.UDPConn, server *net.UDPAddr) (*net.UDPAddr, error) {
	var tx [12]byte
	if _, err := rand.Read(tx[:]); err != nil {
		return nil, err
	}
	request := make([]byte, 20)
	binary.BigEndian.PutUint16(request[0:2], stunBindingRequest)
	binary.BigEndian.PutUint16(request[2:4], 0)
	binary.BigEndian.PutUint32(request[4:8], stunMagicCookie)
	copy(request[8:20], tx[:])
	if _, err := conn.WriteToUDP(request, server); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(p2pDefaultTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)
	buf := make([]byte, 4096)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return nil, err
		}
		if n < 20 || binary.BigEndian.Uint32(buf[4:8]) != stunMagicCookie || !equalBytes(buf[8:20], tx[:]) {
			continue
		}
		if binary.BigEndian.Uint16(buf[0:2]) != stunBindingSuccess {
			continue
		}
		addr, err := parseSTUNMappedAddress(buf[:n])
		if err == nil {
			return addr, nil
		}
	}
}

func parseSTUNMappedAddress(packet []byte) (*net.UDPAddr, error) {
	if len(packet) < 20 {
		return nil, errors.New("short STUN packet")
	}
	length := int(binary.BigEndian.Uint16(packet[2:4]))
	if 20+length > len(packet) {
		return nil, errors.New("invalid STUN message length")
	}
	for offset := 20; offset+4 <= 20+length; {
		attrType := binary.BigEndian.Uint16(packet[offset : offset+2])
		attrLen := int(binary.BigEndian.Uint16(packet[offset+2 : offset+4]))
		valueStart := offset + 4
		valueEnd := valueStart + attrLen
		if valueEnd > len(packet) {
			return nil, errors.New("invalid STUN attribute length")
		}
		if attrType == stunXORMappedAddress || attrType == stunMappedAddress {
			if attrLen < 8 || packet[valueStart+1] != 0x01 {
				return nil, errors.New("unsupported STUN mapped address")
			}
			port := binary.BigEndian.Uint16(packet[valueStart+2 : valueStart+4])
			addrValue := binary.BigEndian.Uint32(packet[valueStart+4 : valueStart+8])
			if attrType == stunXORMappedAddress {
				port ^= uint16(stunMagicCookie >> 16)
				addrValue ^= stunMagicCookie
			}
			ip := net.IPv4(byte(addrValue>>24), byte(addrValue>>16), byte(addrValue>>8), byte(addrValue))
			return &net.UDPAddr{IP: ip, Port: int(port)}, nil
		}
		offset = valueEnd + ((4 - (attrLen % 4)) % 4)
	}
	return nil, errors.New("STUN mapped address was not present")
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
