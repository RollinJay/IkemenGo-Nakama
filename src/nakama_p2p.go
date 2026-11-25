package main

import (
	"context"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ikemen-engine/Ikemen-GO/src/portmap"
)

const (
	// Version 2 added the key exchange: signals carry a key, every datagram
	// is sealed and acks echo their hello's nonce, so a version 1 game cannot
	// complete a handshake with this one, which ends at its signal and says
	// why (HandleSignal).
	p2pMagic = "IKEMEN-P2P/2"
	// p2pMagicV1 marks a game from before version 2 (HandleSignal).
	p2pMagicV1         = "IKEMEN-P2P/1"
	p2pHello           = "hello"
	p2pAck             = "ack"
	p2pDefaultInterval = 75 * time.Millisecond
	// The handshake tries every direct way from the start (punchLoop): the
	// IPv4 path, IPv6 and a port the router opens; and the relays through the
	// server p2pRelayDelay after the peer's signal (its keys) arrived. It
	// gives up when none has worked by the timeout. p2pExtrasDelay only
	// changes what the player is told (Stage).
	p2pDefaultTimeout = 30 * time.Second
	p2pExtrasDelay    = 4 * time.Second
	p2pRelayDelay     = 6 * time.Second
	p2pRelayRetry     = 2 * time.Second
	p2pPortMapTimeout = 5 * time.Second
	// While a session runs on a relay, it keeps checking the direct routes
	// (upgradeLoop): every p2pUpgradeFast for p2pUpgradeFastFor, then every
	// p2pUpgradeSlow; and it signals again every p2pUpgradeResignal and
	// p2pUpgradeResignalSlow after.
	p2pUpgradeFast         = 250 * time.Millisecond
	p2pUpgradeFastFor      = 30 * time.Second
	p2pUpgradeSlow         = 1500 * time.Millisecond
	p2pUpgradeResignal     = time.Second
	p2pUpgradeResignalSlow = 5 * time.Second
	// A relay stays open this long after the session moved to a direct
	// route, so the session can fall back to it at once (p2pMux.checkRoutes),
	// and longer while the peer still sends its game data on it.
	p2pRelayLinger    = 10 * time.Second
	p2pRelayDataQuiet = 3 * time.Second
	// The UDP relay gets this long to carry the handshake before the
	// websocket relay is joined too.
	p2pUDPRelayGrace = 3 * time.Second
	// Port prediction for a peer behind a NAT that maps each destination to
	// a new port (P2PNATInfo): every p2pSprayInterval, for p2pSprayFor after
	// the peer's signal, hellos go to the p2pPredictSeq ports that follow the
	// peer's last mapping by its step, and to p2pPredictRandom random ports
	// within p2pPredictSpread of it.
	p2pSprayInterval  = 250 * time.Millisecond
	p2pSprayFor       = 30 * time.Second
	p2pPredictSeq     = 24
	p2pPredictRandom  = 8
	p2pPredictSpread  = 512
	p2pPredictMaxStep = 64
	p2pPredictedKeep  = 4096
	// A game that completed the handshake waits this long at least for the
	// peer's first data, which shows the peer completed it too (punchLoop).
	p2pConfirmGrace = 10 * time.Second
	// A port the router opened lasts this long unless the session renews it,
	// so one that a game which crashed or lost its network left open does
	// not stay open for long.
	p2pPortMapLifetime   = 15 * time.Minute
	p2pResignalInterval  = 500 * time.Millisecond
	p2pSTUNTimeout       = 3 * time.Second
	p2pSTUNRetry         = 500 * time.Millisecond
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
	// From and To are Nakama user ids. A lobby relays a signal to the member
	// it names, so each signal names its sender and the peer it is meant
	// for; the client takes a signal only when the server names its sender,
	// and replaces From with that name (handleJSON).
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Key is the sender's X25519 public key (hex). The two players' keys give
	// them a secret that nobody can work out from the signals, which keys the
	// sealing of every datagram between them (p2pKeys, p2pCrypt).
	Key string `json:"key"`
	// ServerRelay says the sender's game asks the session's match for the
	// server's relay of the pair (nakama/modules/ikemen_relay.lua) before it
	// falls back to a Nakama relayed match. A game from before it only joins
	// the relayed match, so the two games use the server's relay only when
	// both signals say so.
	ServerRelay bool `json:"server_relay,omitempty"`
	// Purpose says which of a client's sessions the signal is for: "" is the
	// pairing's session (n.p2p), which games from before purposes send, and
	// p2pPurposeWatch a watch link between a match's host and a lobby
	// member who watches it (nakama_watch_p2p.go). A client hands each
	// signal to the session of its purpose only (deliverSignal), and a
	// session takes no signal of another purpose (HandleSignal).
	Purpose string `json:"purpose,omitempty"`
	// NAT is what the sender's STUN queries showed of its NAT, when they
	// showed something (gatherCandidates).
	NAT *P2PNATInfo `json:"nat,omitempty"`
}

// P2PNATInfo describes the sender's NAT from two STUN servers' answers. Type
// is "cone" when both saw the same address (the NAT keeps one port for all
// destinations) and "symmetric" when they did not (a new port for each
// destination: a peer reaches it only at a port the NAT is about to give, so
// the peer predicts it, see sendPunches). IP and Port are the last mapping,
// Delta the step between the two mappings.
type P2PNATInfo struct {
	Type  string `json:"type"`
	IP    string `json:"ip,omitempty"`
	Port  int    `json:"port,omitempty"`
	Delta int    `json:"delta,omitempty"`
}

// p2PPacket is a handshake packet: a hello, which checks a route, or the ack
// that answers it. Like every datagram between the players it travels
// sealed (p2pCrypt).
type p2PPacket struct {
	Magic   string `json:"magic"`
	Kind    string `json:"kind"`
	MatchID string `json:"match_id"`
	Token   string `json:"token"`
	// Nonce names the address a hello was sent to (p2pCheckNonce); an ack
	// echoes the nonce of the hello it answers.
	Nonce string `json:"nonce"`
}

// NakamaP2P owns the UDP socket used for NAT discovery and hole punching.
// After a successful handshake the socket stays open for the whole session:
// a p2pMux shares it between the netplay stream (OpenStream) and GGPO
// (OpenGameplay), so the NAT mapping is created once and kept alive. When
// the IPv4 path does not work, the session can also run over an IPv6 socket
// or a relay through the Nakama server (nakama_p2ppath.go); the mux then
// sends on whichever completed the handshake.
type NakamaP2P struct {
	mu       sync.Mutex
	conn     *net.UDPConn
	udp4     *p2pUDPPath
	conn6    *net.UDPConn
	udp6     *p2pUDPPath
	relayer  p2pRelayer
	relay    *p2pRelayPath
	udpRelay p2pPath // the UDP relay's path once joined
	// relayJoining: a joinRelay runs.
	relayJoining bool
	portMap      *portmap.Mapping
	stage        string
	matchID      string
	token        string
	key          *ecdh.PrivateKey
	remoteToken  string
	remoteKey    string
	keys         *p2pKeys // from the peer's signal; nil before
	// peerServerRelay: the peer's signal says its game asks for the server's
	// relay (P2PSignal.ServerRelay).
	peerServerRelay bool
	// keysReady closes when the peer's first signal gave the keys, at keysAt:
	// the relay's timer counts from then (punchLoop).
	keysReady chan struct{}
	keysOnce  sync.Once
	keysAt    time.Time
	// nat is this side's NAT as its STUN queries showed it, peerNAT the
	// peer's as its signal says; predicted are the ports hellos went to by
	// prediction (sendPunches), and lastSpray when they last did.
	nat        *P2PNATInfo
	peerNAT    *P2PNATInfo
	predicted  map[string]bool
	lastSpray  time.Time
	extrasOnce sync.Once
	// upgrading: an upgradeLoop runs.
	upgrading atomic.Bool
	// crypt seals and opens every datagram on every path once the peer's
	// signal gave the keys (p2pCrypt); the paths hold a pointer to it.
	crypt atomic.Pointer[p2pCrypt]
	// checks are the routes handshake packets from the peer arrived on, at
	// most p2pMaxRoutes (see handshake).
	checks      []*p2pCheck
	remoteAddr  *net.UDPAddr
	local       []P2PCandidate
	remote      []P2PCandidate
	selfID      string
	peerID      string
	purpose     string // P2PSignal.Purpose of this session's signals
	mux         *p2pMux
	readMu      sync.Mutex // held by whichever code is reading conn
	ready       chan struct{}
	closed      chan struct{}
	confirmed   chan struct{} // the peer's first data arrived (p2pMuxHooks.heard)
	oldPeer     bool          // the peer's game speaks version 1 (HandleSignal)
	readyOnce   sync.Once
	closeOnce   sync.Once
	confirmOnce sync.Once
	signal      func(P2PSignal) error
	interval    time.Duration
	timeout     time.Duration
	extrasDelay time.Duration
	relayDelay  time.Duration
	// relayLinger, upgradeFast and upgradeSlow are p2pRelayLinger,
	// p2pUpgradeFast and p2pUpgradeSlow (tests shorten them); muxProbe and
	// muxQuiet, when set, replace the mux's probe interval and quiet time.
	relayLinger time.Duration
	upgradeFast time.Duration
	upgradeSlow time.Duration
	muxProbe    time.Duration
	muxQuiet    time.Duration
	// confirmGrace is how long a game that completed the handshake waits
	// at least for the peer's first data (punchLoop).
	confirmGrace time.Duration
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
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate p2p key: %w", err)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: localPort})
	if err != nil {
		return nil, fmt.Errorf("bind p2p UDP socket: %w", err)
	}
	p := &NakamaP2P{
		conn:         conn,
		stage:        "direct",
		matchID:      matchID,
		token:        hex.EncodeToString(tokenBytes),
		key:          key,
		ready:        make(chan struct{}),
		closed:       make(chan struct{}),
		confirmed:    make(chan struct{}),
		keysReady:    make(chan struct{}),
		predicted:    map[string]bool{},
		relayLinger:  p2pRelayLinger,
		upgradeFast:  p2pUpgradeFast,
		upgradeSlow:  p2pUpgradeSlow,
		signal:       signal,
		interval:     p2pDefaultInterval,
		timeout:      p2pDefaultTimeout,
		extrasDelay:  p2pExtrasDelay,
		relayDelay:   p2pRelayDelay,
		confirmGrace: p2pConfirmGrace,
	}
	p.udp4 = &p2pUDPPath{label: "direct", conn: conn, crypt: &p.crypt}
	return p, nil
}

// SetPeer names this client and the intended peer (Nakama user ids). Signals
// then carry both ids, and signals from anyone else are ignored.
func (p *NakamaP2P) SetPeer(selfID, peerID string) {
	p.mu.Lock()
	p.selfID, p.peerID = selfID, peerID
	p.mu.Unlock()
}

// SetPurpose sets the purpose the session's signals carry and the only one it
// takes signals of (P2PSignal.Purpose); "" for the pairing's session.
func (p *NakamaP2P) SetPurpose(purpose string) {
	p.mu.Lock()
	p.purpose = purpose
	p.mu.Unlock()
}

// SetRelay gives the session the relay it falls back to when no UDP path
// works. Without one, the handshake tries the UDP paths only.
func (p *NakamaP2P) SetRelay(r p2pRelayer) {
	p.mu.Lock()
	p.relayer = r
	p.mu.Unlock()
}

func (p *NakamaP2P) localSignal() P2PSignal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return P2PSignal{Magic: p2pMagic, MatchID: p.matchID, Token: p.token, Key: p.publicKey(),
		Candidates: append([]P2PCandidate(nil), p.local...), From: p.selfID, To: p.peerID, ServerRelay: true, Purpose: p.purpose, NAT: p.nat}
}

// publicKey is this side's half of the key exchange, as signals carry it.
func (p *NakamaP2P) publicKey() string {
	return hex.EncodeToString(p.key.PublicKey().Bytes())
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
	// IPv6 and the router's port from the start, alongside STUN: the first
	// signal carries the IPv6 candidates, and the router's port follows in a
	// signal of its own as soon as the router grants it.
	p.startExtras()
	local, err := p.gatherCandidates(ctx, stunServers)
	if err != nil {
		p.Close()
		return err
	}
	p.mu.Lock()
	p.local = uniqueCandidates(append(local, p.local...))
	p.mu.Unlock()
	if p.signal != nil {
		if err := p.signal(p.localSignal()); err != nil {
			p.Close()
			return err
		}
	}
	go p.punchLoop()
	return nil
}

func (p *NakamaP2P) HandleSignal(signal P2PSignal) error {
	if signal.Magic == p2pMagicV1 && signal.MatchID == p.matchID {
		p.mu.Lock()
		fromPeer := p.peerID != "" && signal.From == p.peerID && (signal.To == "" || signal.To == p.selfID)
		if fromPeer {
			p.oldPeer = true
		}
		p.mu.Unlock()
		if fromPeer {
			// No handshake with it can complete: end at once, rather than
			// after the whole ladder, so the player learns why.
			log.Printf("Nakama P2P: the other player's game is an older version (%s)", p2pMagicV1)
			p.Close()
		}
		return errors.New("the other player's game is an older version")
	}
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
	if signal.Purpose != p.purpose {
		p.mu.Unlock()
		// Meant for another of the client's sessions: a watch link's signal
		// never changes the pairing's session, nor the other way round.
		return nil
	}
	if signal.Token == p.token {
		p.mu.Unlock()
		// Nakama match broadcasts may be delivered to the sender as well.
		// Ignore the local signal instead of treating our own socket as the peer.
		return nil
	}
	if (signal.To != "" && p.selfID != "" && signal.To != p.selfID) ||
		(signal.From != "" && p.peerID != "" && signal.From != p.peerID) {
		p.mu.Unlock()
		// Addressed to, or sent by, another lobby member.
		return nil
	}
	if p.mux != nil && (signal.Token != p.remoteToken || signal.Key != p.remoteKey) {
		// The session is running on the keys it has: a peer that started
		// over needs a new session on this side too.
		p.mu.Unlock()
		return nil
	}
	if signal.Token != p.remoteToken || signal.Key != p.remoteKey {
		// The peer resends its signal until it completes the handshake; the
		// keys change only with a new token or key (another session).
		secret, err := p2pAgree(p.key, signal.Key)
		if err != nil {
			p.mu.Unlock()
			return err
		}
		keys := p2pDeriveKeys(secret, p.matchID, p.token, signal.Token)
		crypt, err := newP2PCrypt(keys.seal, keys.open)
		if err != nil {
			p.mu.Unlock()
			return err
		}
		p.remoteToken, p.remoteKey, p.keys = signal.Token, signal.Key, keys
		p.crypt.Store(crypt)
		if u, ok := p.udpRelay.(interface {
			setCrypt(*atomic.Pointer[p2pCrypt])
		}); ok {
			u.setCrypt(&p.crypt)
		}
		p.keysOnce.Do(func() {
			p.keysAt = time.Now()
			close(p.keysReady)
		})
	}
	p.remote = append([]P2PCandidate(nil), signal.Candidates...)
	p.peerServerRelay = signal.ServerRelay
	if signal.NAT != nil {
		nat := *signal.NAT
		p.peerNAT = &nat
	}
	p.mu.Unlock()
	return nil
}

// p2pAgree completes the key exchange with the key the peer signalled.
func p2pAgree(own *ecdh.PrivateKey, peerKey string) ([]byte, error) {
	b, err := hex.DecodeString(peerKey)
	if err != nil {
		return nil, errors.New("p2p signal has no valid key")
	}
	pub, err := ecdh.X25519().NewPublicKey(b)
	if err != nil {
		return nil, errors.New("p2p signal has no valid key")
	}
	secret, err := own.ECDH(pub)
	if err != nil {
		return nil, errors.New("p2p signal has no valid key")
	}
	return secret, nil
}

// p2pKeys are what the two players derive from the secret of their key
// exchange. Signals carry only public keys, so nobody else can derive them,
// not even other members of a lobby that sees every signal; the server could
// only by forging both players' signals.
type p2pKeys struct {
	handshake []byte // makes the nonces of handshake checks (p2pCheckNonce)
	seal      []byte // seals what this side sends, on every path (p2pCrypt)
	open      []byte // opens what the peer sends
	relayName string // the relayed match the session falls back to
}

// p2pDeriveKeys derives the session's keys from the exchange's secret, the
// match and both handshake tokens. Both players derive the same handshake key
// and relay name; each one's seal key is the other's open key.
func p2pDeriveKeys(secret []byte, matchID, token, remoteToken string) *p2pKeys {
	low, high := token, remoteToken
	if high < low {
		low, high = high, low
	}
	derive := func(purpose string) []byte {
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte("ikemen-p2p-" + purpose + "|" + matchID + "|" + low + "|" + high))
		return mac.Sum(nil)
	}
	// The relayed match a session falls back to without the server's relay
	// (nakama_relay.go): Nakama puts everyone who creates a relayed match of
	// the same name into the same match, so the two players reach it without
	// telling each other its id. Nakama also lists relayed matches and lets
	// anyone join one by its id, so the name is not what keeps the relay
	// private: every datagram on it is sealed, and the client passes on only
	// the peer's data.
	return &p2pKeys{
		handshake: derive("handshake"),
		seal:      derive("from|" + token),
		open:      derive("from|" + remoteToken),
		relayName: "ikemen-p2p-relay-" + hex.EncodeToString(derive("relay-name")[:16]),
	}
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
		// Without IPv4 the ladder still has its other ways: IPv6 and the
		// relay.
		log.Printf("Nakama P2P: no IPv4 address to offer (%v)", err)
	}
	for _, ip := range addresses {
		candidates = append(candidates, P2PCandidate{Type: "host", Address: ip.String(), Port: port})
	}
	// Every STUN server, all at once rather than the first that answers: two
	// answers show whether the NAT keeps one port for all destinations
	// (P2PNATInfo).
	var servers []*net.UDPAddr
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
		servers = append(servers, addr)
	}
	if len(servers) > 0 {
		mapped, err := stunBindings(ctx, conn, servers)
		if err != nil {
			log.Printf("P2P STUN query failed: %v", err)
		}
		var answered []*net.UDPAddr
		for i, m := range mapped {
			if m == nil {
				log.Printf("P2P STUN query failed for %s", servers[i])
				continue
			}
			answered = append(answered, m)
			candidates = append(candidates, P2PCandidate{Type: "srflx", Address: m.IP.String(), Port: m.Port})
		}
		if nat := p2pClassifyNAT(answered); nat != nil {
			log.Printf("Nakama P2P: NAT %s (mapped %v)", nat.Type, answered)
			p.mu.Lock()
			p.nat = nat
			p.mu.Unlock()
		}
	}
	return uniqueCandidates(candidates), nil
}

// p2pClassifyNAT compares the mappings STUN servers saw, in the order the
// queries went out: one address for all means the NAT keeps the port for
// every destination, different ones that it maps each anew. One answer
// shows nothing (nil).
func p2pClassifyNAT(mapped []*net.UDPAddr) *P2PNATInfo {
	if len(mapped) < 2 {
		return nil
	}
	first, last := mapped[0], mapped[len(mapped)-1]
	for _, m := range mapped[1:] {
		if m.Port != first.Port || !m.IP.Equal(first.IP) {
			return &P2PNATInfo{Type: "symmetric", IP: last.IP.String(), Port: last.Port, Delta: last.Port - mapped[len(mapped)-2].Port}
		}
	}
	return &P2PNATInfo{Type: "cone", IP: last.IP.String(), Port: last.Port}
}

// punchLoop runs the handshake until a path to the peer works:
//
//	from the start          the IPv4 path (host, STUN and predicted
//	                        candidates), IPv6 host candidates, and a port the
//	                        router opens to the IPv4 socket (PCP, NAT-PMP or
//	                        UPnP)
//	relayDelay after the    the server's UDP relay, then the relay over the
//	peer's keys arrived     Nakama websocket
//
// and gives up at the timeout. Completing the handshake does not end it:
// the peer may not have completed it yet, and needs this side's hellos and
// acks until it has, possibly on a way that has yet to come (the relay). It
// ends at the peer's first data, which shows the peer completed it, and a
// game that completed it but never hears from the peer gives up. A session
// that runs on a relay then keeps looking for a direct route (upgradeLoop).
func (p *NakamaP2P) punchLoop() {
	timer := time.NewTicker(p.interval)
	defer timer.Stop()
	resignal := time.NewTicker(p2pResignalInterval)
	defer resignal.Stop()
	deadline := time.NewTimer(p.timeout)
	defer deadline.Stop()
	extras := time.NewTimer(p.extrasDelay)
	defer extras.Stop()
	// The relays count from the peer's keys: before them nothing can be
	// sent to the peer on any way, and this side's own start says nothing
	// of when the peer's game started.
	keys := p.keysReady
	var relay <-chan time.Time
	start := time.Now()
	ready := p.ready
	p.startExtras()
	for {
		select {
		case <-p.closed:
			return
		case <-p.confirmed:
			if p2pIsRelay(p.activePath()) {
				p.startUpgrade()
			}
			return
		case <-ready:
			ready = nil
			p.startMux()
			if left := p.timeout - time.Since(start); left < p.confirmGrace {
				deadline.Reset(p.confirmGrace)
			}
		case <-deadline.C:
			if ready == nil {
				log.Printf("Nakama P2P: the other player's game did not finish the handshake")
			} else {
				log.Printf("Nakama P2P: no way to reach the other player worked within %v (direct, IPv6, a router port, the relay)", p.timeout)
			}
			p.Close()
			return
		case <-keys:
			keys = nil
			t := time.NewTimer(p.relayDelay)
			defer t.Stop()
			relay = t.C
		case <-extras.C:
			if ready != nil && p.Stage() == "direct" {
				p.setStage("extras")
				log.Printf("Nakama P2P: no direct path yet after %v; trying IPv6, a port the router opens and predicted ports too", p.extrasDelay)
			}
		case <-relay:
			relay = nil
			p.startRelay()
		case <-resignal.C:
			// Nakama only relays to peers present when the message is sent, so
			// keep publishing until the handshake completes; knowing the peer's
			// token does not mean the peer has received ours.
			p.resignal()
		case <-timer.C:
			p.sendPunches()
		}
	}
}

// activePath is the path the session sends on (nil before it is ready).
func (p *NakamaP2P) activePath() p2pPath {
	p.mu.Lock()
	m := p.mux
	p.mu.Unlock()
	if m == nil {
		return nil
	}
	return m.activeRoute().path
}

// startUpgrade starts an upgradeLoop unless one runs.
func (p *NakamaP2P) startUpgrade() {
	if p.upgrading.CompareAndSwap(false, true) {
		go p.upgradeLoop()
	}
}

// upgradeLoop keeps checking the direct routes while the session runs on a
// relay, or its route went quiet: hellos to every route the mux does not
// know yet (sendPunches), often at first and then less, and the signal
// again now and then, which carries late candidates (the router's port).
// Once a direct route is verified, the mux moves the session to it as soon
// as it answers steadily (p2pMux.recordPong). The loop ends when the session
// runs on a working direct route, or closes.
func (p *NakamaP2P) upgradeLoop() {
	start := time.Now()
	var lastSignal time.Time
	for {
		fast := time.Since(start) < p2pUpgradeFastFor
		wait, signalEvery := p.upgradeSlow, p2pUpgradeResignalSlow
		if fast {
			wait, signalEvery = p.upgradeFast, p2pUpgradeResignal
		}
		select {
		case <-p.closed:
			p.upgrading.Store(false)
			return
		case <-time.After(wait):
		}
		if p.directWorks() {
			p.upgrading.Store(false)
			// One may have started meanwhile, while this one was ending.
			if !p.directWorks() {
				p.startUpgrade()
			}
			return
		}
		p.sendPunches()
		if time.Since(lastSignal) >= signalEvery {
			lastSignal = time.Now()
			p.resignal()
		}
	}
}

// directWorks reports whether the session runs on a direct route that works.
func (p *NakamaP2P) directWorks() bool {
	p.mu.Lock()
	m := p.mux
	p.mu.Unlock()
	return m != nil && !p2pIsRelay(m.activeRoute().path) && m.activeLive()
}

// onSwitch runs when the mux changed the route it sends on: on a relay, the
// session looks for a direct route again; off a relay, it leaves the relays
// after a while (closeRelays).
func (p *NakamaP2P) onSwitch(from, to p2pRoute) {
	if p2pIsRelay(to.path) {
		p.startUpgrade()
		return
	}
	if p2pIsRelay(from.path) {
		p.scheduleRelayClose(p.relayLinger)
	}
}

// onLost runs when the active route went quiet and no other route works:
// the session asks for the relay again (it may have left it) and keeps
// checking the direct routes.
func (p *NakamaP2P) onLost() {
	select {
	case <-p.closed:
		return
	default:
	}
	p.startUpgrade()
	p.startRelay()
}

func (p *NakamaP2P) scheduleRelayClose(after time.Duration) {
	time.AfterFunc(after, p.closeRelays)
}

// closeRelays leaves the relays ranked below the active route, once the
// session runs on a working direct route and the peer sent no game data on
// them for p2pRelayDataQuiet (else it tries again later). Should the direct
// route go quiet later, onLost joins the relay again.
func (p *NakamaP2P) closeRelays() {
	p.mu.Lock()
	m := p.mux
	if m == nil || p.conn == nil {
		p.mu.Unlock()
		return
	}
	active := m.activeRoute()
	if p2pIsRelay(active.path) || !m.activeLive() {
		p.mu.Unlock()
		return
	}
	type relayRoute struct {
		route p2pRoute
		clear func()
	}
	var routes []relayRoute
	if p.relay != nil {
		r := p.relay
		routes = append(routes, relayRoute{p2pRoute{path: r, addr: p2pRelayAddr{peer: p.peerID}}, func() {
			if p.relay == r {
				p.relay = nil
			}
		}})
	}
	if p.udpRelay != nil {
		r := p.udpRelay
		routes = append(routes, relayRoute{p2pRoute{path: r, addr: p2pUDPRelayRouteAddr(r, p.peerID)}, func() {
			if p.udpRelay == r {
				p.udpRelay = nil
			}
		}})
	}
	var closing []relayRoute
	busy := false
	for _, r := range routes {
		if _, data := m.routeActivity(r.route); !data.IsZero() && time.Since(data) < p2pRelayDataQuiet {
			busy = true
			continue
		}
		r.clear()
		closing = append(closing, r)
	}
	p.mu.Unlock()
	for _, r := range closing {
		m.forget(r.route)
		p2pClosePath(r.route.path)
		log.Printf("Nakama P2P: left %s; the session runs on %s", r.route, active)
	}
	if busy {
		p.scheduleRelayClose(p2pRelayDataQuiet)
	}
}

// p2pUDPRelayRouteAddr is the peer's address on the UDP relay path.
func p2pUDPRelayRouteAddr(path p2pPath, peerID string) net.Addr {
	if r, ok := path.(interface{ routeAddr() net.Addr }); ok {
		return r.routeAddr()
	}
	return p2pRelayAddr{peer: peerID}
}

// resignal publishes this side's signal again, candidates or not: the peer
// needs its token and key for every way, the relay included.
func (p *NakamaP2P) resignal() {
	if p.signal != nil {
		_ = p.signal(p.localSignal())
	}
}

// confirm records the peer's first data (p2pMuxHooks.heard).
func (p *NakamaP2P) confirm() {
	p.confirmOnce.Do(func() { close(p.confirmed) })
}

func (p *NakamaP2P) setStage(stage string) {
	p.mu.Lock()
	p.stage = stage
	p.mu.Unlock()
}

// Stage reports the handshake's latest step: "direct", "extras" (no path
// yet after extrasDelay) or "relay" (the relay as well).
func (p *NakamaP2P) Stage() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stage
}

// Route names the path the session runs over once the handshake completed:
// "direct" (the IPv4 socket), "ipv6", "relay-udp" or "relay"; "" before.
// RouteName says more.
func (p *NakamaP2P) Route() string {
	if path := p.activePath(); path != nil {
		return path.name()
	}
	return ""
}

// RouteName names the route the session runs over: "direct-host" (an
// address the peer signalled for its machine), "direct-vpn" (one on a VPN or
// overlay network), "direct-srflx" (its public address as STUN saw it),
// "direct-prflx" (an address only its packets showed), "direct-predicted"
// (a port predicted for its NAT), "direct-router" (a port its router
// opened), "direct-ipv6", "relay-udp" or "relay-ws"; "" before the
// handshake completed.
func (p *NakamaP2P) RouteName() string {
	p.mu.Lock()
	m := p.mux
	p.mu.Unlock()
	if m == nil {
		return ""
	}
	r := m.activeRoute()
	if !r.valid() {
		return ""
	}
	return p.classifyRoute(r)
}

// Upgrades is how often the session changed route after the first one.
func (p *NakamaP2P) Upgrades() int {
	p.mu.Lock()
	m := p.mux
	p.mu.Unlock()
	if m == nil {
		return 0
	}
	return m.switchCount()
}

// classifyRoute names a route as RouteName does.
func (p *NakamaP2P) classifyRoute(r p2pRoute) string {
	if _, ok := r.path.(*p2pRelayPath); ok {
		return "relay-ws"
	}
	if p2pIsRelay(r.path) {
		return "relay-udp"
	}
	addr, ok := r.addr.(*net.UDPAddr)
	if !ok || addr.IP == nil {
		return "direct-prflx"
	}
	if p2pVPNAddress(addr.IP) {
		return "direct-vpn"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.remote {
		ip := net.ParseIP(c.Address)
		if ip == nil || c.Port != addr.Port || !ip.Equal(addr.IP) {
			continue
		}
		switch c.Type {
		case "host":
			return "direct-host"
		case "host6":
			return "direct-ipv6"
		case "srflx":
			return "direct-srflx"
		case "pcp", "natpmp", "upnp":
			return "direct-router"
		}
	}
	if addr.IP.To4() == nil {
		return "direct-ipv6"
	}
	if p.predicted[p2pAddrKey(addr)] {
		return "direct-predicted"
	}
	return "direct-prflx"
}

// p2pVPNNets are the address ranges of the VPN and overlay networks players
// use for netplay: Hamachi (25.0.0.0/8), Radmin VPN (26.0.0.0/8), Tailscale
// (100.64.0.0/10, the shared CGNAT range, and fd7a:115c:a1e0::/48), and
// ZeroTier's usual managed ranges.
var p2pVPNNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, cidr := range []string{"25.0.0.0/8", "26.0.0.0/8", "100.64.0.0/10", "fd7a:115c:a1e0::/48",
		"10.144.0.0/16", "10.147.0.0/16", "10.241.0.0/16", "10.242.0.0/16", "10.243.0.0/16", "10.244.0.0/16", "172.22.0.0/15", "172.24.0.0/14", "fc00::/8"} {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// p2pVPNAdapterNets lists the networks of this machine's VPN adapters (tests
// replace it): an address in one of them is reached over the VPN.
var p2pVPNAdapterNets = sync.OnceValue(vpnAdapterNets)

// p2pVPNAdapterNames are what VPN and overlay adapters' names contain.
var p2pVPNAdapterNames = []string{"hamachi", "radmin", "zerotier", "zt", "tailscale", "wg", "wireguard", "tun", "tap", "utun", "openvpn", "nordlynx"}

func vpnAdapterNets() []*net.IPNet {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []*net.IPNet
	for _, iface := range interfaces {
		name := strings.ToLower(iface.Name)
		vpn := false
		for _, n := range p2pVPNAdapterNames {
			if strings.HasPrefix(name, n) || strings.Contains(name, n+" ") || strings.Contains(name, "vpn") {
				vpn = true
				break
			}
		}
		if !vpn || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				out = append(out, n)
			}
		}
	}
	return out
}

// p2pVPNAddress reports whether ip is on a VPN or overlay network.
func p2pVPNAddress(ip net.IP) bool {
	for _, n := range p2pVPNNets {
		if n.Contains(ip) {
			return true
		}
	}
	for _, n := range p2pVPNAdapterNets() {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// startExtras opens the second and third ways to the peer, once: an IPv6
// socket for IPv6 host candidates, which reach the peer without NAT, and a
// port the router opens to the IPv4 socket, which lets the peer through a
// NAT that drops what it did not ask for. Each new candidate goes out in a
// signal at once.
func (p *NakamaP2P) startExtras() {
	p.extrasOnce.Do(func() {
		if p.openIPv6() {
			p.resignal()
		}
		go p.openRouterPort()
	})
}

// openIPv6 binds the IPv6 socket, on the IPv4 socket's port when it is free,
// and adds this machine's global IPv6 addresses as candidates.
func (p *NakamaP2P) openIPv6() bool {
	addresses, err := p2pIPv6Addresses()
	if err != nil || len(addresses) == 0 {
		log.Printf("Nakama P2P: no IPv6 address to offer (%v)", err)
		return false
	}
	port := p.LocalPort()
	conn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified, Port: port})
	if err != nil {
		conn, err = net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified})
	}
	if err != nil {
		log.Printf("Nakama P2P: could not open an IPv6 socket: %v", err)
		return false
	}
	path := &p2pUDPPath{label: "ipv6", conn: conn, crypt: &p.crypt}
	port = conn.LocalAddr().(*net.UDPAddr).Port
	p.mu.Lock()
	if p.conn == nil {
		p.mu.Unlock()
		_ = conn.Close()
		return false
	}
	p.conn6, p.udp6 = conn, path
	for _, ip := range addresses {
		p.local = append(p.local, P2PCandidate{Type: "host6", Address: ip.String(), Port: port})
	}
	p.local = uniqueCandidates(p.local)
	p.mu.Unlock()
	go p.readPath(path)
	return true
}

// openRouterPort asks the router to forward a UDP port to the IPv4 socket
// and offers the router's public address and that port as a candidate. A
// session that already runs on a direct route does not need it; one that
// runs on a relay keeps it, as a way to move off the relay.
func (p *NakamaP2P) openRouterPort() {
	port := p.LocalPort()
	if port == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), p2pPortMapTimeout)
	defer cancel()
	m, err := p2pMapPort(ctx, port)
	if err != nil {
		log.Printf("Nakama P2P: the router did not open a port: %v", err)
		return
	}
	if path := p.activePath(); path != nil && !p2pIsRelay(path) {
		// A direct way worked meanwhile: the router need not keep the port.
		_ = p2pUnmapPort(m)
		return
	}
	p.mu.Lock()
	if p.conn == nil {
		p.mu.Unlock()
		_ = p2pUnmapPort(m)
		return
	}
	p.portMap = m
	p.local = uniqueCandidates(append(p.local, P2PCandidate{Type: m.Method, Address: m.ExternalIP.String(), Port: m.ExternalPort}))
	p.mu.Unlock()
	log.Printf("Nakama P2P: the router (%s) forwards %s:%d to this session", m.Method, m.ExternalIP, m.ExternalPort)
	p.resignal()
}

// startRelay joins the relays, in the background: joining waits for the
// server's answer, and the handshake goes on meanwhile. It does nothing
// while a join runs or the relays are joined.
func (p *NakamaP2P) startRelay() {
	p.mu.Lock()
	relayer := p.relayer
	if relayer == nil || p.relayJoining || p.relay != nil || p.conn == nil {
		p.mu.Unlock()
		return
	}
	p.relayJoining = true
	p.mu.Unlock()
	p.setStage("relay")
	log.Printf("Nakama P2P: no direct path to the other player yet; also trying the relay through the server")
	go p.joinRelay(relayer)
}

// relayNotNeeded reports whether the session no longer needs to join a
// relay: it closed, or runs on a working direct route.
func (p *NakamaP2P) relayNotNeeded() bool {
	select {
	case <-p.closed:
		return true
	default:
	}
	return p.directWorks()
}

// joinRelay joins the server's UDP relay when the server has one, and the
// relay over the Nakama websocket when the UDP relay does not carry the
// handshake within p2pUDPRelayGrace (or the server has none). It retries
// until joined, or the relay is not needed any more.
func (p *NakamaP2P) joinRelay(relayer p2pRelayer) {
	defer func() {
		p.mu.Lock()
		p.relayJoining = false
		p.mu.Unlock()
	}()
	triedUDP := false
	for {
		if p.relayNotNeeded() {
			return
		}
		p.mu.Lock()
		keys, peerID, server, udpRelay := p.keys, p.peerID, p.peerServerRelay, p.udpRelay
		p.mu.Unlock()
		// The relay's name and keys come from the key exchange, so the peer's
		// signal must have arrived; until then there is nobody to relay to.
		// The signal also says which relay the peer's game joins.
		if keys != nil {
			if ur, ok := relayer.(p2pUDPRelayer); ok && server && !triedUDP && udpRelay == nil {
				triedUDP = true
				if p.joinUDPRelay(ur, peerID) {
					return
				}
			}
			path := &p2pRelayPath{crypt: &p.crypt}
			route := p2pRoute{path: path, addr: p2pRelayAddr{peer: peerID}}
			req := p2pRelayRequest{MatchID: p.matchID, Name: keys.relayName, Peer: peerID, Server: server}
			link, err := relayer.JoinRelay(req, func(b []byte) { p.receive(b, route) })
			if err == nil {
				path.mu.Lock()
				path.link = link
				path.mu.Unlock()
				p.mu.Lock()
				if p.conn == nil {
					p.mu.Unlock()
					path.close()
					return
				}
				p.relay = path
				p.mu.Unlock()
				p.sendPunches()
				return
			}
			log.Printf("Nakama P2P: could not join the relay: %v", err)
		}
		select {
		case <-p.closed:
			return
		case <-time.After(p2pRelayRetry):
		}
	}
}

// joinUDPRelay asks the server for a ticket to its UDP relay and joins it,
// and reports whether the session then runs, or can run, without the
// websocket relay: it waits up to p2pUDPRelayGrace for the handshake to
// complete on some way.
func (p *NakamaP2P) joinUDPRelay(ur p2pUDPRelayer, peerID string) bool {
	ticket, err := ur.RequestUDPRelay(p.matchID, peerID)
	if err != nil {
		log.Printf("Nakama P2P: no UDP relay: %v", err)
		return false
	}
	var route p2pRoute
	var routeMu sync.Mutex
	path, err := p2pDialUDPRelay(ticket, func(b []byte) {
		routeMu.Lock()
		r := route
		routeMu.Unlock()
		if r.valid() {
			p.receive(b, r)
		}
	})
	if err != nil {
		log.Printf("Nakama P2P: could not join the UDP relay: %v", err)
		return false
	}
	if u, ok := path.(interface {
		setCrypt(*atomic.Pointer[p2pCrypt])
	}); ok {
		u.setCrypt(&p.crypt)
	}
	routeMu.Lock()
	route = p2pRoute{path: path, addr: p2pUDPRelayRouteAddr(path, peerID)}
	routeMu.Unlock()
	p.mu.Lock()
	if p.conn == nil {
		p.mu.Unlock()
		p2pClosePath(path)
		return true
	}
	p.udpRelay = path
	p.mu.Unlock()
	log.Printf("Nakama P2P: joined the server's UDP relay")
	p.sendPunches()
	// It carries the handshake once the mux knows its route (verify).
	deadline := time.Now().Add(p2pUDPRelayGrace)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		m := p.mux
		p.mu.Unlock()
		if p.relayNotNeeded() || (m != nil && m.isKnown(route)) {
			return true
		}
		select {
		case <-p.closed:
			return true
		case <-time.After(50 * time.Millisecond):
		}
	}
	return p.relayNotNeeded()
}

// p2pCheckNonce is the nonce of a hello sent to addr. The ack that answers a
// hello echoes its nonce, and counts only when it comes from the address the
// hello went to: it then shows the route works both ways, and a copy of it
// sent from anywhere else shows nothing (ICE's connectivity checks work the
// same way).
func p2pCheckNonce(key []byte, addr net.Addr) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("check|" + p2pAddrKey(addr)))
	return hex.EncodeToString(mac.Sum(nil)[:12])
}

// p2pAddrKey writes an address the same way however it was obtained.
func p2pAddrKey(addr net.Addr) string {
	if u, ok := addr.(*net.UDPAddr); ok {
		ap := u.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()).String()
	}
	return addr.Network() + "|" + addr.String()
}

// handshakePacket builds this side's hello or ack with nonce.
func (p *NakamaP2P) handshakePacket(kind, nonce string) []byte {
	p.mu.Lock()
	matchID, token := p.matchID, p.token
	p.mu.Unlock()
	b, _ := json.Marshal(p2PPacket{Magic: p2pMagic, Kind: kind, MatchID: matchID, Token: token, Nonce: nonce})
	return b
}

// validHandshake returns the handshake packet the peer made for this session
// in data (opened).
func (p *NakamaP2P) validHandshake(data []byte) (p2PPacket, bool) {
	var packet p2PPacket
	if len(data) == 0 || data[0] != '{' {
		return packet, false
	}
	p.mu.Lock()
	matchID, remoteToken := p.matchID, p.remoteToken
	p.mu.Unlock()
	if remoteToken == "" || json.Unmarshal(data, &packet) != nil || packet.Magic != p2pMagic ||
		packet.MatchID != matchID || packet.Token != remoteToken || packet.Nonce == "" ||
		(packet.Kind != p2pHello && packet.Kind != p2pAck) {
		return packet, false
	}
	return packet, true
}

// sendPunches checks every route to the peer with a hello: its candidates,
// the routes its own handshake packets came on, the relays once joined, and
// ports predicted for its NAT (predictTargets). Once the session runs, the
// routes the mux knows are left out: they work, and the mux pings them.
// Before the session is ready it then reads the IPv4 socket for a while.
func (p *NakamaP2P) sendPunches() {
	p.mu.Lock()
	conn, keys := p.conn, p.keys
	udp4, udp6, relay, udpRelay := p.udp4, p.udp6, p.relay, p.udpRelay
	remote := append([]P2PCandidate(nil), p.remote...)
	checks := make([]p2pRoute, 0, len(p.checks))
	for _, c := range p.checks {
		checks = append(checks, c.route)
	}
	peerID := p.peerID
	m := p.mux
	predicted := p.predictTargetsLocked(udp4)
	p.mu.Unlock()
	if conn == nil || keys == nil {
		return
	}
	var targets []p2pRoute
	add := func(r p2pRoute) {
		if m != nil && m.isKnown(r) {
			return
		}
		for _, t := range targets {
			if t.equal(r) {
				return
			}
		}
		targets = append(targets, r)
	}
	for _, candidate := range remote {
		addr := &net.UDPAddr{IP: net.ParseIP(candidate.Address), Port: candidate.Port}
		if addr.IP == nil || addr.Port <= 0 {
			continue
		}
		if addr.IP.To4() != nil {
			add(p2pRoute{path: udp4, addr: addr})
		} else if udp6 != nil {
			add(p2pRoute{path: udp6, addr: addr})
		}
	}
	// The routes the peer's packets came on, which may be none of its
	// candidates: a NAT that gives each destination a port of its own shows
	// the peer to this side at an address only those packets tell (a
	// peer-reflexive candidate, in ICE's words).
	for _, r := range checks {
		if _, udp := r.path.(*p2pUDPPath); udp {
			add(r)
		}
	}
	if udpRelay != nil {
		add(p2pRoute{path: udpRelay, addr: p2pUDPRelayRouteAddr(udpRelay, peerID)})
	}
	if relay != nil {
		add(p2pRoute{path: relay, addr: p2pRelayAddr{peer: peerID}})
	}
	for _, r := range targets {
		_ = r.send(p.handshakePacket(p2pHello, p2pCheckNonce(keys.handshake, r.addr)))
	}
	// Predicted ports are not routes until the peer answers on one, and do
	// not count against p2pMaxRoutes.
	for _, r := range predicted {
		_ = r.send(p.handshakePacket(p2pHello, p2pCheckNonce(keys.handshake, r.addr)))
	}
	_ = p.readHandshake()
}

// predictTargetsLocked returns the predicted ports hellos go to now, when
// the peer signalled a NAT that maps each destination to a new port: its
// mappings toward this side come after the last one STUN saw, by its step,
// on the same address. They go out every p2pSprayInterval, for p2pSprayFor
// after the peer's signal, while the session has no working direct route:
// the sequence after the last mapping, and a few random ports around it
// (the NAT may give ports at random, or others' traffic took some).
func (p *NakamaP2P) predictTargetsLocked(udp4 *p2pUDPPath) []p2pRoute {
	nat := p.peerNAT
	if nat == nil || nat.Type != "symmetric" || udp4 == nil || p.keysAt.IsZero() {
		return nil
	}
	now := time.Now()
	if now.Sub(p.keysAt) > p2pSprayFor || now.Sub(p.lastSpray) < p2pSprayInterval {
		return nil
	}
	if p.mux != nil {
		if a := p.mux.activeRoute(); a.valid() && !p2pIsRelay(a.path) {
			return nil
		}
	}
	ip := net.ParseIP(nat.IP).To4()
	if ip == nil || nat.Port <= 0 || nat.Port > 65535 {
		return nil
	}
	p.lastSpray = now
	step := nat.Delta
	if step == 0 || step > p2pPredictMaxStep || step < -p2pPredictMaxStep {
		step = 1
	}
	var out []p2pRoute
	add := func(port int) {
		if port < 1024 || port > 65535 {
			return
		}
		addr := &net.UDPAddr{IP: ip, Port: port}
		key := p2pAddrKey(addr)
		if !p.predicted[key] && len(p.predicted) < p2pPredictedKeep {
			p.predicted[key] = true
		}
		out = append(out, p2pRoute{path: udp4, addr: addr})
	}
	for k := 1; k <= p2pPredictSeq; k++ {
		add(nat.Port + k*step)
	}
	var rnd [2 * p2pPredictRandom]byte
	_, _ = rand.Read(rnd[:])
	center := nat.Port + p2pPredictSeq*step/2
	for i := 0; i < p2pPredictRandom; i++ {
		off := int(binary.BigEndian.Uint16(rnd[2*i:])) % (2*p2pPredictSpread + 1)
		add(center - p2pPredictSpread + off)
	}
	return out
}

// readHandshake reads the IPv4 socket for a short while, until the mux reads
// it; the other paths have readers of their own (readPath, and the relay's
// deliver function).
func (p *NakamaP2P) readHandshake() error {
	if !p.readMu.TryLock() {
		return nil // the mux is reading the socket
	}
	defer p.readMu.Unlock()
	p.mu.Lock()
	conn := p.conn
	udp4 := p.udp4
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
		// Once the session is ready, by this packet or on another path
		// meanwhile, the mux reads the socket: stop here.
		if p.receive(buf[:n], p2pRoute{path: udp4, addr: addr}) {
			_ = conn.SetReadDeadline(time.Time{})
			return nil
		}
	}
}

// readPath reads a path other than the IPv4 socket for as long as it is
// open: before the handshake completes, for its packets; after, for the mux.
func (p *NakamaP2P) readPath(path *p2pUDPPath) {
	buf := make([]byte, 64*1024)
	for {
		n, from, err := path.conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		p.receive(buf[:n], p2pRoute{path: path, addr: from})
	}
}

// openDatagram opens a datagram the peer sealed (p2pCrypt).
func (p *NakamaP2P) openDatagram(sealed []byte) ([]byte, bool) {
	c := p.crypt.Load()
	if c == nil {
		return nil, false
	}
	return c.open(sealed)
}

// receive takes a sealed datagram from the peer, read on any path but by the
// mux: the mux's once the session is ready, the handshake's before. It
// reports whether the session is ready.
func (p *NakamaP2P) receive(sealed []byte, from p2pRoute) bool {
	data, ok := p.openDatagram(sealed)
	p.mu.Lock()
	m := p.mux
	p.mu.Unlock()
	if !ok {
		return m != nil
	}
	if m != nil {
		m.dispatch(data, from)
		return true
	}
	return p.handshake(data, from)
}

// p2pCheck is what the handshake knows of a route the peer's handshake
// packets arrived on: whether it is verified, and when this side last
// checked it with a hello of its own.
type p2pCheck struct {
	route    p2pRoute
	verified bool
	checked  time.Time
}

// checkLocked returns the entry of route, adding one while there are fewer
// than p2pMaxRoutes, or always with force.
func (p *NakamaP2P) checkLocked(route p2pRoute, force bool) *p2pCheck {
	for _, c := range p.checks {
		if c.route.equal(route) {
			return c
		}
	}
	if len(p.checks) >= p2pMaxRoutes && !force {
		return nil
	}
	c := &p2pCheck{route: route}
	p.checks = append(p.checks, c)
	return c
}

// handshake handles one opened handshake packet that arrived on route,
// before or after the session is ready (the mux hands it those), and reports
// whether the session is ready.
//
// A hello is answered with an ack that echoes its nonce. When the route it
// came on is not verified yet, this side also checks that route with a hello
// of its own (a triggered check): it may be a route only the peer's packets
// show (see sendPunches), and it is the route the peer sends on once its
// ack arrives. An ack verifies the route it arrived on when it echoes the
// nonce of the hello this side sent there: the route then works both ways.
// The first verified route completes the handshake, and the session sends on
// it; a later one takes over when it is better (p2pMux.prefer). The mux takes
// the peer's data only from verified routes, so a copied packet sent from
// elsewhere wins its sender nothing but a hello (recheck).
func (p *NakamaP2P) handshake(data []byte, route p2pRoute) bool {
	packet, ok := p.validHandshake(data)
	p.mu.Lock()
	ready, keys := p.mux != nil, p.keys
	if !ok || keys == nil || p.conn == nil {
		p.mu.Unlock()
		return ready
	}
	if packet.Kind == p2pAck {
		if !hmac.Equal([]byte(packet.Nonce), []byte(p2pCheckNonce(keys.handshake, route.addr))) {
			p.mu.Unlock()
			return ready
		}
		p.mu.Unlock()
		return p.verify(route)
	}
	c := p.checkLocked(route, false)
	if c == nil {
		p.mu.Unlock()
		return ready
	}
	trigger := p.triggerLocked(c)
	p.mu.Unlock()
	_ = route.send(p.handshakePacket(p2pAck, packet.Nonce))
	if trigger {
		_ = route.send(p.handshakePacket(p2pHello, p2pCheckNonce(keys.handshake, route.addr)))
	}
	return ready
}

// triggerLocked reports whether this side checks c's route with a hello of
// its own now: while the route is not verified, at most once an interval.
func (p *NakamaP2P) triggerLocked(c *p2pCheck) bool {
	if c == nil || c.verified || time.Since(c.checked) < p.interval {
		return false
	}
	c.checked = time.Now()
	return true
}

// recheck checks a route the peer's data came on before this side verified
// it (p2pMuxHooks.unverified). The peer sends on a route once its own check
// of it is answered, and moves there from the relay (p2pMux.prefer), maybe
// after both handshakes ended, while this side's triggered check of the
// route was lost. The mux drops the data until the ack to this hello
// verifies the route; like a triggered check, it goes out at most once an
// interval.
func (p *NakamaP2P) recheck(route p2pRoute) {
	p.mu.Lock()
	keys := p.keys
	trigger := keys != nil && p.conn != nil && p.triggerLocked(p.checkLocked(route, false))
	p.mu.Unlock()
	if trigger {
		_ = route.send(p.handshakePacket(p2pHello, p2pCheckNonce(keys.handshake, route.addr)))
	}
}

// verify records that route works both ways (see handshake), and reports
// whether the session is ready.
func (p *NakamaP2P) verify(route p2pRoute) bool {
	p.mu.Lock()
	if p.conn == nil {
		p.mu.Unlock()
		return false
	}
	p.checkLocked(route, true).verified = true
	if m := p.mux; m != nil {
		p.mu.Unlock()
		m.addKnown(route)
		m.consider(route)
		return true
	}
	remote := p.peerIPv4Locked(route)
	p.remoteAddr = remote
	m := newP2PMux(p.conn, p.udp4, remote, route, p2pMuxHooks{
		control:    func(data []byte, from p2pRoute) { p.handshake(data, from) },
		open:       p.openDatagram,
		heard:      p.confirm,
		unverified: p.recheck,
		switched:   p.onSwitch,
		lost:       p.onLost,
	})
	if p.muxProbe > 0 {
		m.probeInterval = p.muxProbe
	}
	if p.muxQuiet > 0 {
		m.routeQuiet = p.muxQuiet
	}
	p.mux = m
	p.mu.Unlock()
	log.Printf("Nakama P2P: connected to the other player over %s", route)
	p.readyOnce.Do(func() { close(p.ready) })
	// At once, not only from punchLoop: the mux reads the socket and pings
	// the peer, whose handshake waits for that first data.
	m.start(&p.readMu)
	return true
}

// OldPeer reports whether the peer's game speaks an older version of the
// handshake, which ended it (HandleSignal).
func (p *NakamaP2P) OldPeer() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.oldPeer
}

// peerIPv4Locked is the peer's address for KCP and GGPO (p2pMux.remote): its
// address on the IPv4 path, or for the other paths the IPv4 address it
// signalled, or else a documentation address (TEST-NET-1, RFC 5737). The
// rollback session takes only an IPv4 peer, and the mux sends every packet
// on the session's route whatever address it names, so nothing is ever sent
// to a stand-in.
func (p *NakamaP2P) peerIPv4Locked(route p2pRoute) *net.UDPAddr {
	if a, ok := route.addr.(*net.UDPAddr); ok && a.IP.To4() != nil {
		c := *a
		return &c
	}
	for _, kind := range []string{"srflx", "pcp", "natpmp", "upnp", "host"} {
		for _, c := range p.remote {
			if c.Type != kind || c.Port <= 0 || c.Port > 65535 {
				continue
			}
			if ip := net.ParseIP(c.Address).To4(); ip != nil {
				return &net.UDPAddr{IP: ip, Port: c.Port}
			}
		}
	}
	return &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 9}
}

func (p *NakamaP2P) startMux() {
	p.mu.Lock()
	m := p.mux
	p.mu.Unlock()
	if m != nil {
		m.start(&p.readMu)
	}
}

func (p *NakamaP2P) readyMux() (*p2pMux, error) {
	select {
	case <-p.ready:
	default:
		return nil, errors.New("p2p connection is not ready")
	}
	select {
	case <-p.closed:
		return nil, errors.New("p2p connection closed")
	default:
	}
	p.mu.Lock()
	m := p.mux
	p.mu.Unlock()
	if m == nil {
		return nil, errors.New("p2p connection is not ready")
	}
	m.start(&p.readMu)
	return m, nil
}

// OpenStream opens the reliable netplay stream to the peer. Both peers derive
// the same KCP conversation id from the two handshake tokens.
func (p *NakamaP2P) OpenStream() (net.Conn, error) {
	m, err := p.readyMux()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	conv := p2pConv(p.token, p.remoteToken, "stream")
	p.mu.Unlock()
	return openP2PStream(m, conv)
}

// OpenGameplay returns a fresh GGPO channel on the shared socket and the peer
// address to send to. Each rollback match takes a new one; closing it leaves
// the socket and the netplay stream open.
func (p *NakamaP2P) OpenGameplay() (net.PacketConn, *net.UDPAddr, error) {
	m, err := p.readyMux()
	if err != nil {
		return nil, nil, err
	}
	ch, err := m.attach(p2pTagGGPO, 0)
	if err != nil {
		return nil, nil, err
	}
	return ch, m.remoteAddr(), nil
}

// RTT returns the round trip to the peer measured over the punched path
// (smoothed), and the number of pings behind it; 0, 0 before the handshake
// or while the peer does not answer pings.
func (p *NakamaP2P) RTT() (time.Duration, int) {
	p.mu.Lock()
	m := p.mux
	p.mu.Unlock()
	if m == nil {
		return 0, 0
	}
	return m.rtt()
}

// State reports the handshake state: connecting, ready, failed or closed.
func (p *NakamaP2P) State() string {
	ready, closed := false, false
	select {
	case <-p.ready:
		ready = true
	default:
	}
	select {
	case <-p.closed:
		closed = true
	default:
	}
	switch {
	case ready && closed:
		return "closed"
	case ready:
		return "ready"
	case closed:
		return "failed"
	default:
		return "connecting"
	}
}

func (p *NakamaP2P) Close() {
	p.closeOnce.Do(func() {
		close(p.closed)
		p.mu.Lock()
		conn, conn6, relay, udpRelay, portMap := p.conn, p.conn6, p.relay, p.udpRelay, p.portMap
		p.conn, p.conn6, p.relay, p.udpRelay, p.portMap = nil, nil, nil, nil, nil
		p.mu.Unlock()
		if udpRelay != nil {
			p2pClosePath(udpRelay)
		}
		if conn != nil {
			_ = conn.Close()
		}
		if conn6 != nil {
			_ = conn6.Close()
		}
		if relay != nil {
			relay.close()
		}
		if portMap != nil {
			// Closing the router's port talks to the router: not on the
			// caller's time.
			go p2pUnmapPort(portMap)
		}
	})
}

// p2pRouteProbe is the address the fallback in localIPv4Addresses asks a
// route for. 192.0.2.1 is reserved for documentation (TEST-NET-1, RFC 5737),
// so it should not be in use on a real network and normally takes the default
// route, which is the route traffic to STUN servers and peers takes too. No
// packet is sent to it, and the port is not used.
const p2pRouteProbe = "192.0.2.1:9"

// p2pInterfaceAddresses lists this machine's IPv4 addresses from its network
// interfaces, and p2pRouteAddress returns the local address the operating
// system picks for traffic to the internet (tests replace both).
var (
	p2pInterfaceAddresses = interfaceIPv4Addresses
	p2pRouteAddress       = routeIPv4Address
)

// p2pIPv6Addresses lists this machine's global IPv6 addresses for IPv6 host
// candidates, p2pMapPort asks the router to forward a UDP port to the IPv4
// socket, p2pUnmapPort closes that port again, and p2pCloseAllPorts closes
// every port still open (tests replace them).
var (
	p2pIPv6Addresses = globalIPv6Addresses
	p2pMapPort       = func(ctx context.Context, port int) (*portmap.Mapping, error) {
		return portmap.Map(ctx, port, portmap.Options{Description: "IKEMEN GO netplay", Lifetime: p2pPortMapLifetime, Logf: log.Printf})
	}
	p2pUnmapPort     = func(m *portmap.Mapping) error { return m.Close() }
	p2pCloseAllPorts = portmap.CloseAll
)

// releaseRouterPorts asks the routers to close the ports P2P sessions had
// them open (openRouterPort); the game calls it as it exits. A port it cannot
// close in time closes at the end of its lifetime (p2pPortMapLifetime).
func releaseRouterPorts() {
	p2pCloseAllPorts(1500 * time.Millisecond)
}

// p2pRouteProbe6 is p2pRouteProbe for IPv6: 2001:db8::/32 is reserved for
// documentation (RFC 3849).
const p2pRouteProbe6 = "[2001:db8::1]:9"

// globalIPv6Addresses returns the global unicast IPv6 addresses of the
// network interfaces that are up, sorted: not link-local, and not unique
// local (fc00::/7), which do not reach the internet. Like
// localIPv4Addresses, it falls back to the address the operating system
// would send internet traffic from when the interfaces cannot be listed.
func globalIPv6Addresses() ([]net.IP, error) {
	var out []net.IP
	interfaces, err := net.Interfaces()
	if err == nil {
		seen := map[string]bool{}
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
				if globalIPv6(ip) && !seen[ip.String()] {
					seen[ip.String()] = true
					out = append(out, ip)
				}
			}
		}
	}
	if len(out) == 0 {
		if conn, derr := net.Dial("udp6", p2pRouteProbe6); derr == nil {
			if ip := netAddrIP(conn.LocalAddr()); globalIPv6(ip) {
				out = append(out, ip)
			}
			conn.Close()
		}
	}
	if len(out) == 0 && err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

func globalIPv6(ip net.IP) bool {
	return ip != nil && ip.To4() == nil && ip.IsGlobalUnicast() && !ip.IsPrivate()
}

// localIPv4Addresses returns the addresses for host candidates. It lists the
// network interfaces' addresses; when that fails or finds none, it falls back
// to the address the operating system would send internet traffic from.
// Android 11 and later do not let apps that target API level 30 or later
// list interfaces: net.Interfaces fails with "netlinkrib: permission denied"
// (golang/go#40569). When the fallback fails too, the result is what the
// interface listing returned.
func localIPv4Addresses() ([]net.IP, error) {
	addresses, err := p2pInterfaceAddresses()
	if err == nil && len(addresses) > 0 {
		return addresses, nil
	}
	local, routeErr := p2pRouteAddress()
	ip := netAddrIP(local)
	if routeErr == nil && ip != nil {
		ip = ip.To4()
		// A peer cannot reach a loopback or unspecified (0.0.0.0) address.
		if ip != nil && !ip.IsUnspecified() && !ip.IsLoopback() {
			if err != nil {
				log.Printf("P2P could not list network interfaces (%v); using the address %s instead", err, ip)
			}
			return []net.IP{ip}, nil
		}
	}
	return addresses, err
}

// routeIPv4Address returns the local address of a UDP socket connected to
// p2pRouteProbe. Connecting a UDP socket only makes the operating system
// choose the route and the local address for it; no packet is sent, and it
// needs no permission beyond network access.
func routeIPv4Address() (net.Addr, error) {
	conn, err := net.Dial("udp4", p2pRouteProbe)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return conn.LocalAddr(), nil
}

// interfaceIPv4Addresses returns the IPv4 addresses of the network interfaces
// that are up, other than loopback, sorted.
func interfaceIPv4Addresses() ([]net.IP, error) {
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
	mapped, err := stunBindings(ctx, conn, []*net.UDPAddr{server})
	if err != nil {
		return nil, err
	}
	if mapped[0] == nil {
		return nil, fmt.Errorf("no STUN answer from %s", server)
	}
	return mapped[0], nil
}

// stunBindings asks every server for this socket's mapped address at once,
// in order, and returns each one's answer (nil for a server that did not
// answer in time).
func stunBindings(ctx context.Context, conn *net.UDPConn, servers []*net.UDPAddr) ([]*net.UDPAddr, error) {
	requests := make([][]byte, len(servers))
	for i := range servers {
		request := make([]byte, 20)
		binary.BigEndian.PutUint16(request[0:2], stunBindingRequest)
		binary.BigEndian.PutUint16(request[2:4], 0)
		binary.BigEndian.PutUint32(request[4:8], stunMagicCookie)
		if _, err := rand.Read(request[8:20]); err != nil {
			return nil, err
		}
		requests[i] = request
	}
	mapped := make([]*net.UDPAddr, len(servers))
	deadline := time.Now().Add(p2pSTUNTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	defer conn.SetReadDeadline(time.Time{})
	buf := make([]byte, 4096)
	left := len(servers)
	// UDP requests get lost; resend the unanswered ones until the deadline.
	for left > 0 && time.Now().Before(deadline) {
		for i, server := range servers {
			if mapped[i] == nil {
				if _, err := conn.WriteToUDP(requests[i], server); err != nil {
					return mapped, err
				}
			}
		}
		retry := time.Now().Add(p2pSTUNRetry)
		if retry.After(deadline) {
			retry = deadline
		}
		_ = conn.SetReadDeadline(retry)
		for left > 0 {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break
				}
				return mapped, err
			}
			if n < 20 || binary.BigEndian.Uint32(buf[4:8]) != stunMagicCookie || binary.BigEndian.Uint16(buf[0:2]) != stunBindingSuccess {
				continue
			}
			for i := range servers {
				if mapped[i] != nil || !equalBytes(buf[8:20], requests[i][8:20]) {
					continue
				}
				if addr, err := parseSTUNMappedAddress(buf[:n]); err == nil {
					mapped[i] = addr
					left--
				}
			}
		}
	}
	return mapped, nil
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
