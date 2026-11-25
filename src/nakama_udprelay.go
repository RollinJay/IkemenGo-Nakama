package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// The UDP relay: the fallback of a session whose games find no direct path,
// ahead of the relay over the Nakama websocket, which rides TCP and stutters
// when a segment is lost. A separate program forwards the datagrams
// (nakama/udp-relay); the server module hands each of the two players a
// ticket for it (the ikemen_relay RPC with udp=true, ikemen_relay.lua). Each
// game joins the relay with its token from a UDP socket of its own, and the
// relay forwards each one's data to the other. The datagrams are sealed
// (p2pCrypt) like on every path: the relay cannot read or change them.
//
// Wire format (nakama/udp-relay/relay): game -> relay 'J'+token['|'+cookie]
// (join), 'D'+datagram, 'K'+0 (keepalive); relay -> game 'C'+cookie (join
// again with it: the address must prove it receives), 'A'+state (1 waiting,
// 2 paired), 'E'+code (refused), 'D'+datagram.

const (
	udpRelayMsgJoin      = 'J'
	udpRelayMsgData      = 'D'
	udpRelayMsgKeepalive = 'K'
	udpRelayMsgAck       = 'A'
	udpRelayMsgError     = 'E'
	udpRelayMsgCookie    = 'C'
	udpRelayStatePaired  = 2
	udpRelayErrNotJoined = 4
	udpRelayCookieLen    = 32

	// udpRelayDialTimeout bounds the wait for the relay's first answer.
	udpRelayDialTimeout = 3 * time.Second
	// udpRelayJoinEvery: joins are sent again this often until the other
	// side is there too (a join can be lost), for udpRelayJoinFastFor, and
	// every udpRelayJoinSlow after; udpRelayKeepEvery keeps the NAT's
	// mapping and the relay's side alive after. The relay answers
	// keepalives and joins: a path that heard nothing from the relay for
	// udpRelayDeadAfter is dead (the relay forgot it, or went away), and the
	// session asks for a new ticket (NakamaP2P.joinRelay).
	udpRelayJoinEvery   = 300 * time.Millisecond
	udpRelayJoinFastFor = 10 * time.Second
	udpRelayJoinSlow    = 2 * time.Second
	udpRelayKeepEvery   = 5 * time.Second
	udpRelayDeadAfter   = 20 * time.Second
	// udpRelayMaxDatagram: the relay forwards at most 2048 bytes.
	udpRelayMaxDatagram = 2048
	udpRelayMaxToken    = 256
)

// udpRelayTicket is the server's leave to use the UDP relay: its address,
// the token that joins this player's side of the session until about Expires
// (Unix seconds on this machine's clock: the server's TTL counted from when
// its answer arrived), the other player (Peer) and the session's id. The
// relay decides when the token expires; Expires is only informative.
type udpRelayTicket struct {
	Addr    string
	Token   string
	Expires int64
	Peer    string
	Session string
}

// errNakamaNoUDPRelay: the server has no UDP relay (it answered with its
// websocket relay, or not at all).
var errNakamaNoUDPRelay = errors.New("the server has no UDP relay")

// RequestUDPRelay asks the session's match (matchID) for a ticket to the
// server's UDP relay for this player and peer: the ikemen_relay RPC with
// udp=true. errNakamaNoUDPRelay when the server has none (it then answers as
// it always did, with its websocket relay, which the caller can join with
// JoinRelay), errNakamaNoServerRelay on a server without the module.
func (n *NakamaClient) RequestUDPRelay(matchID, peer string) (udpRelayTicket, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nakamaRelayJoinTimeout)
	defer cancel()
	body, err := n.CallRPC(ctx, nakamaRelayRPC, map[string]any{"match_id": matchID, "peer_id": peer, "udp": true})
	if err != nil {
		if nakamaRPCMissing(err) {
			return udpRelayTicket{}, errNakamaNoServerRelay
		}
		return udpRelayTicket{}, fmt.Errorf("the server's UDP relay: %w", err)
	}
	t, err := parseUDPRelayReply(body, peer, n.cfg.Host, time.Now())
	if err != nil {
		return udpRelayTicket{}, err
	}
	return t, nil
}

// parseUDPRelayReply reads the RPC's answer {udp: {addr, token, expires,
// ttl, session}}. An addr without a host (":17360") is on the Nakama
// server's host (serverHost). The client's clock may be off, so a ticket is
// never refused for its expiry here: the relay checks it.
func parseUDPRelayReply(body []byte, peer, serverHost string, now time.Time) (udpRelayTicket, error) {
	var reply struct {
		UDP *struct {
			Addr    string          `json:"addr"`
			Token   string          `json:"token"`
			Expires json.RawMessage `json:"expires"`
			TTL     json.RawMessage `json:"ttl"`
			Session string          `json:"session"`
		} `json:"udp"`
	}
	if json.Unmarshal(body, &reply) != nil {
		return udpRelayTicket{}, fmt.Errorf("the server's answer names no UDP relay: %.200s", body)
	}
	if reply.UDP == nil {
		return udpRelayTicket{}, errNakamaNoUDPRelay
	}
	u := reply.UDP
	// Lua numbers come as 1.7e9 or 1700000000.
	exp, err := strconv.ParseFloat(string(u.Expires), 64)
	if err != nil || u.Addr == "" || u.Token == "" || len(u.Token) > udpRelayMaxToken {
		return udpRelayTicket{}, fmt.Errorf("the server's UDP relay ticket is malformed: %.200s", body)
	}
	host, port, err := net.SplitHostPort(u.Addr)
	if err != nil || port == "" {
		return udpRelayTicket{}, fmt.Errorf("the server's UDP relay address %q: %v", u.Addr, err)
	}
	if host == "" {
		host = serverHost
	}
	t := udpRelayTicket{Addr: net.JoinHostPort(host, port), Token: u.Token, Expires: int64(exp), Peer: peer, Session: u.Session}
	if ttl, err := strconv.ParseFloat(string(u.TTL), 64); err == nil && ttl > 0 {
		t.Expires = now.Unix() + int64(ttl)
	}
	return t, nil
}

// p2pUDPRelayAddr stands for the peer on the UDP relay path.
type p2pUDPRelayAddr struct{ session string }

func (a p2pUDPRelayAddr) Network() string { return "udp-relay" }
func (a p2pUDPRelayAddr) String() string  { return "relay-udp:" + a.session }

// p2pUDPRelayPath is a session's path through the UDP relay: a socket of its
// own, connected to the relay, so nothing else reaches it. It seals what it
// sends with the session's keys (setCrypt), and gives onPacket every
// datagram from the other side as it came, sealed, like the websocket relay
// path's deliver.
type p2pUDPRelayPath struct {
	conn     *net.UDPConn
	ticket   udpRelayTicket
	crypt    atomic.Pointer[atomic.Pointer[p2pCrypt]]
	onPacket func([]byte)
	join     []byte
	paired   atomic.Bool
	acked    chan struct{}
	refused  chan byte
	done     chan struct{}
	once     sync.Once
	sent     atomic.Int64 // unix nanoseconds of the last datagram sent
	recv     atomic.Int64 // unix nanoseconds of the last datagram from the relay
	// unpairedAt is when the path last found itself without the other side
	// (unix nanoseconds): joins go out often at first, then less.
	unpairedAt atomic.Int64
	// deadWhy says why the path died (isDead), once.
	deadWhy atomic.Pointer[string]
}

// dialUDPRelay joins the UDP relay with ticket t, and returns the path once
// the relay accepted the join; the other side may join later (paired). The
// path's writes fail with errP2PNoKeys until setCrypt gives it the session's
// keys. close ends it.
func dialUDPRelay(t udpRelayTicket, onPacket func([]byte)) (p2pPath, error) {
	p, err := dialUDPRelayPath(t, onPacket)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func dialUDPRelayPath(t udpRelayTicket, onPacket func([]byte)) (*p2pUDPRelayPath, error) {
	if t.Token == "" || len(t.Token) > udpRelayMaxToken {
		return nil, errors.New("udp relay: no token")
	}
	raddr, err := net.ResolveUDPAddr("udp", t.Addr)
	if err != nil {
		return nil, fmt.Errorf("udp relay address %q: %w", t.Addr, err)
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return nil, fmt.Errorf("udp relay: %w", err)
	}
	p := &p2pUDPRelayPath{
		conn:     conn,
		ticket:   t,
		onPacket: onPacket,
		acked:    make(chan struct{}),
		refused:  make(chan byte, 1),
		done:     make(chan struct{}),
	}
	join := append([]byte{udpRelayMsgJoin}, t.Token...)
	p.join = join
	now := time.Now().UnixNano()
	p.recv.Store(now)
	p.unpairedAt.Store(now)
	go p.read()
	deadline := time.NewTimer(udpRelayDialTimeout)
	defer deadline.Stop()
	resend := time.NewTicker(udpRelayJoinEvery)
	defer resend.Stop()
	for {
		_, _ = conn.Write(join)
		select {
		case <-p.acked:
			go p.keepalive(join)
			return p, nil
		case code := <-p.refused:
			p.close()
			return nil, fmt.Errorf("udp relay refused the ticket (code %d)", code)
		case <-deadline.C:
			p.close()
			return nil, fmt.Errorf("udp relay %s did not answer within %v", t.Addr, udpRelayDialTimeout)
		case <-resend.C:
		}
	}
}

// setCrypt gives the path the session's keys (NakamaP2P.crypt).
func (p *p2pUDPRelayPath) setCrypt(c *atomic.Pointer[p2pCrypt]) { p.crypt.Store(c) }

func (p *p2pUDPRelayPath) name() string { return "relay-udp" }

// routeAddr is the peer's address on this path, for a p2pRoute.
func (p *p2pUDPRelayPath) routeAddr() net.Addr { return p2pUDPRelayAddr{session: p.ticket.Session} }

// isPaired reports whether the other side has joined the relay too.
func (p *p2pUDPRelayPath) isPaired() bool { return p.paired.Load() }

// isDead reports whether the path stopped working for good: closed, refused
// by the relay (its ticket expired, or the relay is full), or the relay went
// silent for udpRelayDeadAfter. The session then needs a new ticket.
func (p *p2pUDPRelayPath) isDead() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// die ends the path, saying why once.
func (p *p2pUDPRelayPath) die(why string) {
	if p.deadWhy.CompareAndSwap(nil, &why) {
		log.Printf("Nakama P2P: the UDP relay path stopped: %s", why)
	}
	p.close()
}

func (p *p2pUDPRelayPath) write(b []byte, _ net.Addr) error {
	select {
	case <-p.done:
		return net.ErrClosed
	default:
	}
	sealed, err := sealFor(p.crypt.Load(), b)
	if err != nil {
		return err
	}
	out := make([]byte, 1+len(sealed))
	out[0] = udpRelayMsgData
	copy(out[1:], sealed)
	if len(out) > udpRelayMaxDatagram {
		return errors.New("udp relay: datagram too large")
	}
	p.sent.Store(time.Now().UnixNano())
	_, err = p.conn.Write(out)
	return err
}

func (p *p2pUDPRelayPath) read() {
	buf := make([]byte, udpRelayMaxDatagram+1)
	ackOnce := sync.Once{}
	for {
		n, err := p.conn.Read(buf)
		if err != nil {
			select {
			case <-p.done:
				return
			default:
			}
			// ICMP port unreachable on a connected socket, before the
			// relay runs: keep reading until closed.
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if n < 2 || n > udpRelayMaxDatagram {
			continue
		}
		p.recv.Store(time.Now().UnixNano())
		switch buf[0] {
		case udpRelayMsgData:
			if p.onPacket != nil {
				p.onPacket(append([]byte(nil), buf[1:n]...))
			}
		case udpRelayMsgCookie:
			// The relay asks this address to prove it receives: the join
			// again, with the cookie.
			if n == 1+udpRelayCookieLen {
				out := make([]byte, 0, len(p.join)+1+udpRelayCookieLen)
				out = append(append(append(out, p.join...), '|'), buf[1:n]...)
				_, _ = p.conn.Write(out)
			}
		case udpRelayMsgAck:
			if buf[1] == udpRelayStatePaired {
				if !p.paired.Swap(true) {
					log.Printf("Nakama P2P: the other player joined the UDP relay")
				}
			} else if p.paired.Swap(false) {
				// The relay forgot the other side (it left, or idled).
				p.unpairedAt.Store(time.Now().UnixNano())
			}
			ackOnce.Do(func() { close(p.acked) })
		case udpRelayMsgError:
			if buf[1] == udpRelayErrNotJoined {
				// The relay does not know this address (the NAT mapped
				// the socket anew, or the relay forgot it): join again.
				// The relay decides whether the ticket still joins.
				_, _ = p.conn.Write(p.join)
				continue
			}
			select {
			case <-p.acked:
				// Refused after the dial (the ticket expired on a rejoin,
				// or the address is another session's): the path is dead.
				p.die(fmt.Sprintf("the relay refused it (code %d)", buf[1]))
				return
			default:
			}
			select {
			case p.refused <- buf[1]:
			default:
			}
		}
	}
}

// keepalive joins again until the other side is there (a join or the
// relay's word of the pairing can be lost), then keeps the NAT's mapping and
// the relay's side alive while nothing else goes out.
func (p *p2pUDPRelayPath) keepalive(join []byte) {
	t := time.NewTicker(udpRelayJoinEvery)
	defer t.Stop()
	var lastKeep time.Time
	for {
		select {
		case <-p.done:
			return
		case now := <-t.C:
			if now.UnixNano()-p.recv.Load() >= int64(udpRelayDeadAfter) {
				p.die(fmt.Sprintf("nothing from the relay for %v", udpRelayDeadAfter))
				return
			}
			if !p.paired.Load() {
				// Often at first, then every udpRelayJoinSlow: the other
				// side may never come.
				every := udpRelayJoinEvery
				if now.UnixNano()-p.unpairedAt.Load() >= int64(udpRelayJoinFastFor) {
					every = udpRelayJoinSlow
				}
				if now.UnixNano()-p.sent.Load() >= int64(every) {
					p.sent.Store(now.UnixNano())
					_, _ = p.conn.Write(join)
				}
				continue
			}
			// When nothing went out, or nothing came, for a while: the relay
			// answers, which shows it still has this side.
			quiet := now.UnixNano()-p.sent.Load() >= int64(udpRelayKeepEvery) ||
				now.UnixNano()-p.recv.Load() >= int64(udpRelayKeepEvery)
			if quiet && now.Sub(lastKeep) >= udpRelayKeepEvery {
				lastKeep = now
				p.sent.Store(now.UnixNano())
				_, _ = p.conn.Write([]byte{udpRelayMsgKeepalive, 0})
			}
		}
	}
}

// close ends the path.
func (p *p2pUDPRelayPath) close() {
	p.once.Do(func() {
		close(p.done)
		_ = p.conn.Close()
	})
}
