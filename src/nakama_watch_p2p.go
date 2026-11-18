package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Direct watch links: a lobby match's stream sent peer to peer.
//
// The host of a lobby match (the pairing's player 1) publishes the match's
// replay stream through the lobby, which relays it to every member but the
// two players and keeps it for late watchers (nakama_watch.go,
// ikemen_lobby.lua relay_stream). A member who watches the match also opens
// a watch link: a P2P session of its own to the host (purpose "watch", so it
// never touches the pairing's session, n.p2p), with a reliable stream on it.
// The host sends on it the current match's segment headers and the newest
// messages it kept (watchLog, watchBackfill), paced (sendBackfill), and then
// every message it publishes. The rest of the match the watcher already has:
// the lobby relays everything to it until it says the link carries the
// stream. The watcher stores what comes exactly as the lobby's copies
// (applyReplayMessage), which keeps one copy of each stream and frame, so
// receiving both is safe.
//
// Once the link carries the stream (a header arrived), the watcher tells the
// lobby ("watch_p2p" on), which then stops relaying the stream to it: the
// server's upload is spared, and the host's is about 6 KB/s per watcher.
// When the link ends, lost or stopped, the watcher tells the lobby
// ("watch_p2p" off) and, while the match it carried is still the current
// one, asks for the stream from its start ("watch"), which fills what neither
// path delivered meanwhile. A link that never connects changes nothing: the
// lobby's relay goes on. The watcher may start its link before the host
// publishes (the lobby starts it with the stream): the host refuses the
// signals until it serves the match, and the watcher keeps signalling, with
// new sessions, for watchViewerRetryFor. So a direct path is used where one
// works, and the server's is the fallback.
//
// Watch links take the direct ways of the ladder only (IPv4, IPv6, a port the
// router opens): the server relay is for the two players (the lobby lets only
// them use it), and for a watcher it would only be a second way through the
// server. The router port mappings of several sessions are independent (the
// portmap package tracks and releases each one), so watch links keep that
// rung. A host serves at most watchLinkMax watchers directly; the others
// watch through the lobby.

const (
	// p2pPurposeWatch marks the signals of watch links (P2PSignal.Purpose).
	p2pPurposeWatch = "watch"
	// watchLinkMax is the number of watchers a host sends the stream to
	// directly; more would take too much of its upload during its match.
	watchLinkMax = 8
	// What a host keeps of its match's stream for the watchers who connect
	// later: the lobby's limits (ikemen_lobby.lua STREAM_MESSAGE_LIMIT and
	// STREAM_BYTES_LIMIT), about 20 minutes of play.
	watchLogMessages = 2500
	watchLogBytes    = 8 << 20
	// watchLinkQueueBytes is how far a watcher's link may fall behind
	// (the whole kept stream and then some); a link further behind is
	// dropped, and its watcher goes back to the lobby's relay.
	watchLinkQueueBytes = watchLogBytes + 2<<20
	// watchFrameMax bounds one frame on a link; the lobby's own limits are
	// 16 KB for a header and 8 KB for the others.
	watchFrameMax = 1 << 20
	// watchWriteTimeout ends a link whose watcher stopped taking data.
	watchWriteTimeout = 10 * time.Second
	// watchCheckInterval: how often a link checks that it is still wanted.
	watchCheckInterval = time.Second
	// watchLinkLinger keeps a host's link open this long after the host
	// stopped being the current match's player 1: the lobby ends the match
	// at its result, while the watcher is still the watch delay behind and
	// the last messages may still be on their way.
	watchLinkLinger = 30 * time.Second
	// watchFrameHeaderSize: a frame's length (4 bytes, big endian, counting
	// the op code and the body) and op code (4 bytes, the lobby's op code of
	// the message), then the message's JSON.
	watchFrameHeaderSize = 8
	// watchBackfillTail is how much of the newest kept messages a new link
	// gets after the headers: those the lobby may not have relayed yet when
	// the watcher's "watch_p2p" reaches it (several seconds of play).
	watchBackfillTail = 32 << 10
	// watchBackfillRate paces what a new link gets before the live messages
	// (bytes a second, and the burst): the host is playing its own match,
	// whose packets must not wait behind it. One link at a time does.
	watchBackfillRate = 48 << 10
	// A watcher whose link does not connect (the host does not serve the
	// match yet, or no direct way works) tries again with a new session
	// after watchViewerRetryDelay, until watchViewerRetryFor after it
	// started; it watches through the lobby meanwhile and afterwards.
	watchViewerRetryFor   = 90 * time.Second
	watchViewerRetryDelay = 2 * time.Second
)

// watchFrameOp is the op code of an encoded frame.
func watchFrameOp(frame []byte) uint32 {
	if len(frame) < watchFrameHeaderSize {
		return 0
	}
	return binary.BigEndian.Uint32(frame[4:8])
}

// watchBackfill picks what a new link gets of the kept frames: every header
// (the watcher needs them to tell the lobby, and to have the segments of a
// Turns match chained) and the newest frames up to watchBackfillTail, in
// their order. The older messages the lobby has already relayed.
func watchBackfill(frames [][]byte) [][]byte {
	tail, size := len(frames), 0
	for tail > 0 && size+len(frames[tail-1]) <= watchBackfillTail {
		tail--
		size += len(frames[tail])
	}
	var out [][]byte
	for i, f := range frames {
		if i >= tail || watchFrameOp(f) == nakamaReplayHeaderOp {
			out = append(out, f)
		}
	}
	return out
}

// encodeWatchFrame frames one replay stream message for a watch link.
func encodeWatchFrame(op uint32, body []byte) []byte {
	frame := make([]byte, watchFrameHeaderSize+len(body))
	binary.BigEndian.PutUint32(frame[0:4], uint32(4+len(body)))
	binary.BigEndian.PutUint32(frame[4:8], op)
	copy(frame[watchFrameHeaderSize:], body)
	return frame
}

// readWatchFrame reads one frame from a watch link: the message's op code
// and JSON.
func readWatchFrame(r io.Reader) (uint32, []byte, error) {
	var head [watchFrameHeaderSize]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(head[0:4])
	if size < 4 || size-4 > watchFrameMax {
		return 0, nil, fmt.Errorf("watch link frame of %d bytes", size)
	}
	body := make([]byte, size-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return binary.BigEndian.Uint32(head[4:8]), body, nil
}

// watchLog is what a host keeps of its match's stream, as frames, for the
// watchers who connect during the match: from the match's first header on,
// within the lobby's limits. Past them, like the lobby, it keeps nothing
// more (a watcher who connects then could not get the whole match) and lets
// go of what it kept.
type watchLog struct {
	frames    [][]byte
	bytes     int
	truncated bool
}

// add keeps a frame; start (the header of a match's first stream) begins a
// new match.
func (l *watchLog) add(frame []byte, start bool) {
	if start {
		*l = watchLog{}
	}
	if l.truncated {
		return
	}
	if len(l.frames) >= watchLogMessages || l.bytes+len(frame) > watchLogBytes {
		*l = watchLog{truncated: true}
		return
	}
	l.frames = append(l.frames, frame)
	l.bytes += len(frame)
}

// watchHostLink is a host's link to one watcher.
type watchHostLink struct {
	peer    string
	matchID string
	p       *NakamaP2P
	// subscribed: the link gets every frame published from now on. Set,
	// with the kept frames queued, under nakamaWatchP2P.mu, so no frame
	// is queued twice or missed.
	subscribed bool
	mu         sync.Mutex
	queue      [][]byte
	queued     int
	stream     net.Conn
	wake       chan struct{}
	connected  atomic.Bool
	done       chan struct{}
	doneOnce   sync.Once
	closeOnce  sync.Once
}

// markDone ends the link at once without waiting for its session to close
// (close does that): nothing is queued to it any more, and its goroutines
// stop. Safe under nakamaWatchP2P.mu and on the game thread.
func (l *watchHostLink) markDone() {
	l.doneOnce.Do(func() { close(l.done) })
	l.mu.Lock()
	l.queue, l.queued = nil, 0
	l.mu.Unlock()
}

// push queues frames to send; false when the link is too far behind.
func (l *watchHostLink) push(frames ...[]byte) bool {
	l.mu.Lock()
	for _, f := range frames {
		l.queue = append(l.queue, f)
		l.queued += len(f)
	}
	over := l.queued > watchLinkQueueBytes
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return !over
}

func (l *watchHostLink) take() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	frames := l.queue
	l.queue, l.queued = nil, 0
	return frames
}

func (l *watchHostLink) close() {
	l.markDone()
	l.closeOnce.Do(func() {
		l.mu.Lock()
		stream := l.stream
		l.mu.Unlock()
		if stream != nil {
			_ = stream.Close()
		}
		if l.p != nil {
			l.p.Close()
		}
	})
}

func (l *watchHostLink) isDone() bool {
	select {
	case <-l.done:
		return true
	default:
		return false
	}
}

// watchViewerLink is a watcher's link to the host of the match it watches.
type watchViewerLink struct {
	host    string
	matchID string
	// receiving: a header came over the link, and the lobby was told to
	// stop relaying the stream to this member.
	receiving atomic.Bool
	// seq is the lobby's seq of the match whose header came over the link
	// last (0 when unknown).
	seq atomic.Int64
	// stopped: the link was closed on purpose (StopWatchLink), not lost.
	stopped atomic.Bool
	mu      sync.Mutex
	// p is the link's current session: a new one replaces it at each try
	// (runViewer), under mu.
	p         *NakamaP2P
	stream    net.Conn
	done      chan struct{}
	closeOnce sync.Once
}

func (v *watchViewerLink) session() *NakamaP2P {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.p
}

// setSession makes p the link's session for a new try; false (and p is left
// to the caller) when the link was closed meanwhile.
func (v *watchViewerLink) setSession(p *NakamaP2P) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.isDone() {
		return false
	}
	old := v.p
	v.p = p
	if old != nil {
		old.Close()
	}
	return true
}

func (v *watchViewerLink) close() {
	v.closeOnce.Do(func() {
		close(v.done)
		v.mu.Lock()
		stream, p := v.stream, v.p
		v.mu.Unlock()
		if stream != nil {
			_ = stream.Close()
		}
		if p != nil {
			p.Close()
		}
	})
}

func (v *watchViewerLink) isDone() bool {
	select {
	case <-v.done:
		return true
	default:
		return false
	}
}

// nakamaWatchP2P runs a client's watch links, as a host and as a watcher.
// Its methods are safe on nil (a client made without NewNakamaClient). It
// is never called with the client's lock held: it calls the client's
// methods, which take it (its own lock may be held while it does).
type nakamaWatchP2P struct {
	n   *NakamaClient
	mu  sync.Mutex
	log watchLog
	// logSeq is the lobby's seq of the match the log keeps (0: unknown);
	// a link gets the log only while it is the current match's.
	logSeq int64
	// holeSeq is the lobby's seq of a match whose link this client stopped
	// on purpose while it carried the stream (0: none): what was published
	// between the link's end and the lobby's taking over came by neither
	// path. A later watch of that match asks the lobby for it from its start
	// (replayStatus().hole_seq); the "watch" command clears it.
	holeSeq int64
	hosts   map[string]*watchHostLink
	viewer  *watchViewerLink
	// backfillSlot lets one host link at a time send its backfill.
	backfillSlot chan struct{}
	// Tests replace these: tune adjusts a new session (shorter ladder
	// steps), command sends a lobby command; the pacing and the watcher's
	// tries.
	tune         func(*NakamaP2P)
	command      func(kind string, fields map[string]any) error
	backfillRate int
	retryFor     time.Duration
	retryDelay   time.Duration
}

func newNakamaWatchP2P(n *NakamaClient) *nakamaWatchP2P {
	return &nakamaWatchP2P{n: n, hosts: map[string]*watchHostLink{}, backfillSlot: make(chan struct{}, 1),
		backfillRate: watchBackfillRate, retryFor: watchViewerRetryFor, retryDelay: watchViewerRetryDelay}
}

func (w *nakamaWatchP2P) selfID() string {
	if session := w.n.Session(); session != nil {
		return session.UserID
	}
	return ""
}

func (w *nakamaWatchP2P) sendCommand(kind string, fields map[string]any) {
	send := w.command
	if send == nil {
		send = w.n.SendLobbyCommand
	}
	if err := send(kind, fields); err != nil {
		log.Printf("Nakama watch link: could not send the lobby %q: %v", kind, err)
	}
}

// newSession makes a watch link's P2P session to peer: on a port of its
// own (the pairing's session may hold the configured one), with the watch
// purpose, and without the server relay (see above).
func (w *nakamaWatchP2P) newSession(matchID, peer string) (*NakamaP2P, error) {
	p, err := NewNakamaP2P(matchID, 0, func(s P2PSignal) error { return w.n.PublishSignal(s) })
	if err != nil {
		return nil, err
	}
	p.SetPurpose(p2pPurposeWatch)
	p.SetPeer(w.selfID(), peer)
	if w.tune != nil {
		w.tune(p)
	}
	return p, nil
}

func (w *nakamaWatchP2P) stunServers() []string {
	w.n.mu.Lock()
	defer w.n.mu.Unlock()
	return append([]string(nil), w.n.stunServers...)
}

// lobbyCurrent reads the lobby state's current match: its players, seq and
// whether its stream has started. ok is false outside a lobby or a match.
func lobbyCurrent(state map[string]any) (player1, player2 string, seq int64, streaming, ok bool) {
	cur, _ := state["current"].(map[string]any)
	if cur == nil {
		return "", "", 0, false, false
	}
	player1, _ = cur["player1"].(string)
	player2, _ = cur["player2"].(string)
	if f, isNum := cur["seq"].(float64); isNum {
		seq = int64(f)
	}
	streaming, _ = cur["watch"].(bool)
	return player1, player2, seq, streaming, player1 != ""
}

// lobbyHasMember reports whether the lobby state lists the member.
func lobbyHasMember(state map[string]any, userID string) bool {
	members, _ := state["members"].([]any)
	for _, m := range members {
		if member, _ := m.(map[string]any); member != nil && member["user_id"] == userID {
			return true
		}
	}
	return false
}

// hostedSeq is the lobby's seq of the current match when this client hosts
// it (its player 1), else 0.
func (w *nakamaWatchP2P) hostedSeq() int64 {
	state, _, ok := w.n.LobbyState()
	if !ok {
		return 0
	}
	player1, _, seq, _, ok := lobbyCurrent(state)
	if !ok || player1 == "" || player1 != w.selfID() {
		return 0
	}
	return seq
}

// mayServe reports whether this client, as the host of the lobby's current
// match, sends its stream to the member from: this client is the match's
// player 1 and publishes, and from is another member of the lobby but not
// the match's other player (who plays it). It returns the lobby's match id.
func (w *nakamaWatchP2P) mayServe(from string) (string, bool) {
	self := w.selfID()
	matchID := w.n.CurrentMatchID()
	if from == "" || self == "" || from == self || matchID == "" || w.n.ReplayPublish().Disabled {
		return "", false
	}
	state, _, ok := w.n.LobbyState()
	if !ok {
		return "", false
	}
	player1, player2, _, _, ok := lobbyCurrent(state)
	if !ok || player1 != self || from == player2 || !lobbyHasMember(state, from) {
		return "", false
	}
	return matchID, true
}

// handleSignal takes a watch link's signal (deliverSignal checked whom it
// is for and who sent it): the watcher's link to its sender, or the host's
// link from its sender, which a first signal opens when this client hosts
// the sender's match. A host that does not serve the match yet ignores the
// signal; the watcher signals again (NakamaP2P.resignal, runViewer).
func (w *nakamaWatchP2P) handleSignal(signal P2PSignal) {
	if w == nil || signal.From == "" {
		return
	}
	w.mu.Lock()
	if v := w.viewer; v != nil && v.host == signal.From && !v.isDone() {
		w.mu.Unlock()
		if p := v.session(); p != nil {
			_ = p.HandleSignal(signal)
		}
		return
	}
	if l := w.hosts[signal.From]; l != nil && !l.isDone() {
		w.mu.Unlock()
		_ = l.p.HandleSignal(signal)
		return
	}
	w.mu.Unlock()
	matchID, ok := w.mayServe(signal.From)
	if !ok || signal.MatchID != matchID {
		return
	}
	p, err := w.newSession(matchID, signal.From)
	if err != nil {
		log.Printf("Nakama watch link: %v", err)
		return
	}
	link := &watchHostLink{peer: signal.From, matchID: matchID, p: p, wake: make(chan struct{}, 1), done: make(chan struct{})}
	w.mu.Lock()
	if l := w.hosts[signal.From]; l != nil && !l.isDone() {
		// Another signal of the same watcher opened its link meanwhile.
		w.mu.Unlock()
		p.Close()
		_ = l.p.HandleSignal(signal)
		return
	}
	active := 0
	for _, l := range w.hosts {
		if !l.isDone() {
			active++
		}
	}
	if active >= watchLinkMax {
		w.mu.Unlock()
		p.Close()
		return // the watcher goes on watching through the lobby
	}
	w.hosts[signal.From] = link
	w.mu.Unlock()
	if err := p.HandleSignal(signal); err != nil {
		w.endHost(link)
		return
	}
	go w.runHost(link)
}

func (w *nakamaWatchP2P) endHost(link *watchHostLink) {
	link.close()
	w.mu.Lock()
	if w.hosts[link.peer] == link {
		delete(w.hosts, link.peer)
	}
	w.mu.Unlock()
}

// runHost completes a host's link and sends the stream on it until it ends.
func (w *nakamaWatchP2P) runHost(link *watchHostLink) {
	defer w.endHost(link)
	go w.watchHost(link)
	if err := link.p.Start(context.Background(), w.stunServers()); err != nil {
		return
	}
	if err := link.p.Wait(context.Background()); err != nil {
		return
	}
	stream, err := link.p.OpenStream()
	if err != nil {
		return
	}
	link.mu.Lock()
	link.stream = stream
	link.mu.Unlock()
	if link.isDone() {
		_ = stream.Close()
		return
	}
	// What the watcher needs of the kept frames (when they are the current
	// match's: the log holds the previous one until the new one's first
	// header), then every frame published from now on, both taken under
	// one lock, so no frame is missed or sent twice.
	w.mu.Lock()
	var backfill [][]byte
	if seq := w.hostedSeq(); seq != 0 && w.logSeq == seq {
		backfill = watchBackfill(w.log.frames)
	}
	link.subscribed = true
	w.mu.Unlock()
	link.connected.Store(true)
	log.Printf("Nakama watch link: sending the match to %s directly (%s)", link.peer, link.p.Route())
	go func() {
		// The watcher sends nothing on the stream; reading shows when it
		// ends (closed, or the peer fell silent: p2pMux.keepalive).
		buf := make([]byte, 512)
		for {
			if _, err := stream.Read(buf); err != nil {
				link.close()
				return
			}
		}
	}()
	if !w.sendBackfill(link, stream, backfill) {
		return
	}
	for {
		for _, frame := range link.take() {
			_ = stream.SetWriteDeadline(time.Now().Add(watchWriteTimeout))
			if _, err := stream.Write(frame); err != nil {
				return
			}
		}
		select {
		case <-link.done:
			return
		case <-link.wake:
		}
	}
}

// sendBackfill sends a new link's backfill, paced: a token bucket of
// backfillRate bytes a second (and as much burst), one link at a time. The
// frames published meanwhile wait in the link's queue. False when the link
// ended.
func (w *nakamaWatchP2P) sendBackfill(link *watchHostLink, stream net.Conn, frames [][]byte) bool {
	if len(frames) == 0 {
		return !link.isDone()
	}
	select {
	case w.backfillSlot <- struct{}{}:
	case <-link.done:
		return false
	}
	defer func() { <-w.backfillSlot }()
	rate := float64(w.backfillRate)
	if rate <= 0 {
		rate = watchBackfillRate
	}
	tokens, last := rate, time.Now()
	for _, frame := range frames {
		// A frame larger than the burst goes once the bucket is full.
		need := min(float64(len(frame)), rate)
		for {
			now := time.Now()
			tokens = min(rate, tokens+rate*now.Sub(last).Seconds())
			last = now
			if tokens >= need {
				break
			}
			wait := time.Duration((need - tokens) / rate * float64(time.Second))
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-link.done:
				timer.Stop()
				return false
			}
		}
		tokens -= float64(len(frame))
		_ = stream.SetWriteDeadline(time.Now().Add(watchWriteTimeout))
		if _, err := stream.Write(frame); err != nil {
			return false
		}
	}
	return !link.isDone()
}

// watchHost closes a host's link that is no longer wanted: the client left
// the lobby, the watcher did, the watcher plays the current match (it is
// the next pairing's player 1 or 2: it must not get its own match), or the
// client has not been the current match's player 1 for watchLinkLinger.
func (w *nakamaWatchP2P) watchHost(link *watchHostLink) {
	ticker := time.NewTicker(watchCheckInterval)
	defer ticker.Stop()
	served := time.Now()
	for {
		select {
		case <-link.done:
			return
		case <-ticker.C:
		}
		if w.n.CurrentMatchID() != link.matchID {
			link.close()
			return
		}
		if state, _, ok := w.n.LobbyState(); ok {
			if !lobbyHasMember(state, link.peer) {
				link.close()
				return
			}
			if player1, player2, _, _, ok := lobbyCurrent(state); ok && (link.peer == player1 || link.peer == player2) {
				link.close()
				return
			}
		}
		if _, ok := w.mayServe(link.peer); ok {
			served = time.Now()
		} else if time.Since(served) > watchLinkLinger {
			link.close()
			return
		}
	}
}

// publish sends a stream message the client publishes to its watchers'
// links and keeps it for those who connect later; start marks the header
// of a match's first stream. The lobby gets the message as before. It runs
// on the game thread: a link too far behind is ended at once and closed in
// the background.
func (w *nakamaWatchP2P) publish(op uint32, body []byte, start bool) {
	if w == nil {
		return
	}
	frame := encodeWatchFrame(op, body)
	start = start && op == nakamaReplayHeaderOp
	var seq int64
	if start {
		seq = w.hostedSeq()
	}
	var behind []*watchHostLink
	w.mu.Lock()
	w.log.add(frame, start)
	if start {
		w.logSeq = seq
	}
	for peer, l := range w.hosts {
		if l.subscribed && !l.push(frame) {
			l.markDone()
			delete(w.hosts, peer)
			behind = append(behind, l)
		}
	}
	w.mu.Unlock()
	for _, l := range behind {
		log.Printf("Nakama watch link: %s is too far behind; it watches through the lobby again", l.peer)
		go l.close()
	}
}

// HostLinks is the number of watchers this client sends a stream to
// directly.
func (w *nakamaWatchP2P) HostLinks() int {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	count := 0
	for _, l := range w.hosts {
		if l.connected.Load() && !l.isDone() {
			count++
		}
	}
	return count
}

// StartWatchLink opens this client's watch link to host, the host of the
// lobby match it watches (the pairing's player 1). It returns at once; the
// link connects in the background, and the stream keeps coming through the
// lobby until it does. It may start before the host publishes the match:
// the link keeps trying for watchViewerRetryFor. A link to the same host
// that is still open (or trying) is kept.
func (w *nakamaWatchP2P) StartWatchLink(host string) error {
	if w == nil {
		return errors.New("nakama is not connected")
	}
	self := w.selfID()
	matchID := w.n.CurrentMatchID()
	switch {
	case host == "":
		return errors.New("the match's host is unknown")
	case self == "":
		return errors.New("nakama is not connected")
	case host == self:
		return errors.New("a player cannot watch its own match")
	case matchID == "":
		return errors.New("no lobby is joined")
	}
	w.mu.Lock()
	if v := w.viewer; v != nil && v.host == host && v.matchID == matchID && !v.isDone() {
		w.mu.Unlock()
		return nil
	}
	old := w.viewer
	w.viewer = nil
	w.mu.Unlock()
	if old != nil {
		old.stopped.Store(true)
		old.close()
	}
	p, err := w.newSession(matchID, host)
	if err != nil {
		return err
	}
	v := &watchViewerLink{host: host, matchID: matchID, p: p, done: make(chan struct{})}
	w.mu.Lock()
	w.viewer = v
	w.mu.Unlock()
	go w.runViewer(v)
	return nil
}

// StopWatchLink closes this client's watch link; the lobby relays the
// stream to it again.
func (w *nakamaWatchP2P) StopWatchLink() {
	if w == nil {
		return
	}
	w.mu.Lock()
	v := w.viewer
	w.viewer = nil
	w.mu.Unlock()
	if v != nil {
		v.stopped.Store(true)
		v.close()
	}
}

// ViewerTransport says how the stream this client watches arrives: "p2p"
// over its watch link (once a header came over it), "server" through the
// lobby (also while the link is still connecting or trying).
func (w *nakamaWatchP2P) ViewerTransport() string {
	if w == nil {
		return "server"
	}
	w.mu.Lock()
	v := w.viewer
	w.mu.Unlock()
	if v != nil && v.receiving.Load() && !v.isDone() {
		return "p2p"
	}
	return "server"
}

// viewerWanted reports whether a watcher's link that has not connected
// should try again: the client is still in the lobby, and host is still
// the current match's player 1 and this client not one of its players.
func (w *nakamaWatchP2P) viewerWanted(v *watchViewerLink) bool {
	if v.isDone() || w.n.CurrentMatchID() != v.matchID {
		return false
	}
	state, _, ok := w.n.LobbyState()
	if !ok {
		return true // the state is on its way; the tries are bounded anyway
	}
	player1, player2, _, _, ok := lobbyCurrent(state)
	self := w.selfID()
	return ok && player1 == v.host && self != player1 && self != player2
}

// connectViewer runs one try of a watcher's link: its session's handshake,
// then its stream; nil when the try failed.
func (w *nakamaWatchP2P) connectViewer(v *watchViewerLink) net.Conn {
	p := v.session()
	if p == nil {
		return nil
	}
	if err := p.Start(context.Background(), w.stunServers()); err != nil {
		return nil
	}
	if err := p.Wait(context.Background()); err != nil {
		return nil
	}
	stream, err := p.OpenStream()
	if err != nil {
		return nil
	}
	v.mu.Lock()
	v.stream = stream
	v.mu.Unlock()
	if v.isDone() {
		_ = stream.Close()
		return nil
	}
	return stream
}

// runViewer completes a watcher's link and stores what comes over it. A
// link that does not connect (each session gives up at its handshake
// timeout; a host that does not serve the match yet ignores the signals)
// tries again with a new session while it is wanted, for retryFor, and
// then ends quietly: the stream keeps coming through the lobby.
func (w *nakamaWatchP2P) runViewer(v *watchViewerLink) {
	defer w.endViewer(v)
	go w.watchViewer(v)
	begun := time.Now()
	var stream net.Conn
	for {
		if stream = w.connectViewer(v); stream != nil {
			break
		}
		if v.isDone() || time.Since(begun)+w.retryDelay >= w.retryFor || !w.viewerWanted(v) {
			return
		}
		timer := time.NewTimer(w.retryDelay)
		select {
		case <-v.done:
			timer.Stop()
			return
		case <-timer.C:
		}
		if !w.viewerWanted(v) {
			return
		}
		p, err := w.newSession(v.matchID, v.host)
		if err != nil {
			log.Printf("Nakama watch link: %v", err)
			return
		}
		if !v.setSession(p) {
			p.Close()
			return
		}
	}
	p := v.session()
	for {
		op, body, err := readWatchFrame(stream)
		if err != nil {
			return
		}
		payload, err := w.n.applyReplayMessage(op, body)
		if err != nil {
			log.Printf("Nakama watch link: %v", err)
			continue
		}
		if op == nakamaReplayHeaderOp {
			// The link carries the stream from a header on: the lobby can stop
			// relaying it. Told at every header, since a new match clears the
			// lobby's record of it (start_match).
			if state, _, ok := w.n.LobbyState(); ok {
				if player1, _, seq, _, ok := lobbyCurrent(state); ok && player1 == v.host {
					v.seq.Store(seq)
				}
			}
			if !v.receiving.Swap(true) {
				log.Printf("Nakama watch link: receiving the match from %s directly (%s)", v.host, p.Route())
			}
			w.sendCommand("watch_p2p", map[string]any{"on": true})
		}
		w.n.emit(NakamaEvent{Type: replayEventType(op), MatchID: v.matchID, Payload: payload})
	}
}

// watchViewer closes a watcher's link when the client left the lobby.
func (w *nakamaWatchP2P) watchViewer(v *watchViewerLink) {
	ticker := time.NewTicker(watchCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-v.done:
			return
		case <-ticker.C:
		}
		if w.n.CurrentMatchID() != v.matchID {
			v.stopped.Store(true)
			v.close()
			return
		}
	}
}

// endViewer ends a watcher's link. When the link carried the stream, the
// lobby relays it again. A lost link also has the lobby send the match again
// from its start, while it is still the current one, which fills what neither
// path delivered in between. A link stopped on purpose only records the gap
// (holeSeq): the watcher stopped watching, and a resend now would cost the
// lobby the whole match (or an error, for one too long to keep) for nothing.
func (w *nakamaWatchP2P) endViewer(v *watchViewerLink) {
	v.close()
	w.mu.Lock()
	if w.viewer == v {
		w.viewer = nil
	}
	w.mu.Unlock()
	if !v.receiving.Load() || w.n.CurrentMatchID() != v.matchID {
		return
	}
	w.sendCommand("watch_p2p", map[string]any{"on": false})
	if !v.stopped.Load() {
		log.Printf("Nakama watch link: the link to %s ended; the match comes through the lobby again", v.host)
	}
	if state, _, ok := w.n.LobbyState(); ok {
		player1, _, seq, streaming, ok := lobbyCurrent(state)
		if carried := v.seq.Load(); ok && streaming && player1 == v.host && (carried == 0 || carried == seq) {
			if v.stopped.Load() {
				w.mu.Lock()
				w.holeSeq = seq
				w.mu.Unlock()
			} else {
				w.sendCommand("watch", map[string]any{"seq": seq})
			}
		}
	}
}

// HoleSeq is the seq of the match a deliberately stopped link left a gap in
// (0: none).
func (w *nakamaWatchP2P) HoleSeq() int64 {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.holeSeq
}

// noteWatchCommand clears the gap once the lobby is asked for that match from
// its start.
func (w *nakamaWatchP2P) noteWatchCommand(fields map[string]any) {
	if w == nil {
		return
	}
	var seq int64
	switch v := fields["seq"].(type) {
	case int64:
		seq = v
	case int:
		seq = int64(v)
	case float64:
		seq = int64(v)
	}
	w.mu.Lock()
	if seq != 0 && seq == w.holeSeq {
		w.holeSeq = 0
	}
	w.mu.Unlock()
}

// closeAll closes every watch link: the client left the lobby (keepLog
// false: the kept stream goes too) or lost the connection (keepLog true:
// the match may go on over the players' P2P path, and the links of a
// reconnected client need its start). The sessions close in the
// background: each takes a moment, and the caller may be the game's.
func (w *nakamaWatchP2P) closeAll(keepLog bool) {
	if w == nil {
		return
	}
	w.mu.Lock()
	hosts := w.hosts
	w.hosts = map[string]*watchHostLink{}
	v := w.viewer
	w.viewer = nil
	if !keepLog {
		w.log, w.logSeq = watchLog{}, 0
	}
	for _, l := range hosts {
		l.markDone()
	}
	w.mu.Unlock()
	if v != nil {
		v.stopped.Store(true)
	}
	go func() {
		for _, l := range hosts {
			l.close()
		}
		if v != nil {
			v.close()
		}
	}()
}
