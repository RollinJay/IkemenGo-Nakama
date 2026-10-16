package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Ranked claims that wait for a final answer (nakama/README.md, Ranked
// sets: "Claims sent again").
//
// When a ranked set is decided, the game claims its winner with the server
// (nakama.submitRankedSet). The server's answer is final when it says what
// became of the set: settled, disputed, void, or a refusal of the claim.
// "open" (the other player's claim has not come) is not final, and neither
// is a claim the server did not answer (no connection, the server stopped).
// Such a claim is kept here, in memory and in save/ranked_pending.json, so
// that quitting the game does not lose it, and sent again: 5 seconds after
// the first send, 15 seconds after the second, then every 60 seconds, at
// once whenever the game connects to the server again, and when the game
// next starts (online.lua asks for it), until the server's answer is final
// or the claim is a day old. Past the server's claim window
// (IKEMEN_RANKED_CLAIM_MINUTES) the server's answer is final: the set is
// disputed or void by then.
//
// The file: {"version": 1, "claims": [...]}, each claim {set_id, match_id,
// set, winner, user_id, server: {host, port, ssl, server_key, device_id},
// created, sends}: the set's id and the claim itself, the account that
// played the set, the server it was played on (device_id only when the
// account's device id came from the server profile rather than from
// save/nakama_device_id), when it was first sent (Unix seconds) and how many
// times it was sent. A claim is sent again only as the account that played
// the set. A file that cannot be read is ignored (and replaced at the next
// change).

const (
	rankedPendingVersion = 1
	// A claim is kept at most this long after its first send.
	rankedPendingMaxAge = 24 * time.Hour
)

// rankedRetryDelay is how long after the sends-th send of a claim it is sent
// again: 5 seconds after the first, 15 after the second, 60 after the others.
func rankedRetryDelay(sends int) time.Duration {
	switch {
	case sends <= 0:
		return 0
	case sends == 1:
		return 5 * time.Second
	case sends == 2:
		return 15 * time.Second
	}
	return 60 * time.Second
}

// rankedPendingServer is the server a set was played on, and the device id
// of the account (when it is not the installation's own).
type rankedPendingServer struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	SSL       bool   `json:"ssl,omitempty"`
	ServerKey string `json:"server_key"`
	DeviceID  string `json:"device_id,omitempty"`
}

func (s rankedPendingServer) key() string {
	return fmt.Sprintf("%s|%d|%t|%s|%s", s.Host, s.Port, s.SSL, s.ServerKey, s.DeviceID)
}

// rankedPendingClaim is a claim of the file.
type rankedPendingClaim struct {
	SetID   string              `json:"set_id"`
	MatchID string              `json:"match_id"`
	Set     int                 `json:"set"`
	Winner  string              `json:"winner"`
	UserID  string              `json:"user_id"`
	Server  rankedPendingServer `json:"server"`
	Created int64               `json:"created"`
	Sends   int                 `json:"sends"`
}

func (c rankedPendingClaim) claim() NakamaRankedClaim {
	return NakamaRankedClaim{MatchID: c.MatchID, Set: c.Set, Winner: c.Winner}
}

func (c rankedPendingClaim) expired(now time.Time) bool {
	return now.Sub(time.Unix(c.Created, 0)) >= rankedPendingMaxAge
}

type rankedPendingData struct {
	Version int                  `json:"version"`
	Claims  []rankedPendingClaim `json:"claims"`
}

// readRankedPending reads the claims of the file at path: those not a day
// old, and those that are (expired). A missing file has none; a file that
// cannot be read returns an error, and no claims.
func readRankedPending(path string, now time.Time) (claims, expired []rankedPendingClaim, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var file rankedPendingData
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if file.Version != rankedPendingVersion {
		return nil, nil, fmt.Errorf("%s: version %d, not %d", path, file.Version, rankedPendingVersion)
	}
	for _, c := range file.Claims {
		if c.SetID == "" || c.MatchID == "" || c.Winner == "" || c.UserID == "" || c.Server.Host == "" {
			continue
		}
		if c.expired(now) {
			expired = append(expired, c)
		} else {
			claims = append(claims, c)
		}
	}
	return claims, expired, nil
}

// writeRankedPending writes claims to the file at path (a new file put in
// place of the old one), or removes the file when there are none.
func writeRankedPending(path string, claims []rankedPendingClaim) error {
	if len(claims) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(rankedPendingData{Version: rankedPendingVersion, Claims: claims}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// rankedAnswer is what became of one send of a claim: status is the
// server's (settled, disputed, open), or for a refusal, what became of a set
// that takes no more claims (void, disputed or settled) or refused (the set
// takes none, or not this one), error when the server did not answer,
// expired for a claim a day old that is no longer sent. final: no more
// sends.
type rankedAnswer struct {
	SetID  string
	Status string
	Final  bool
	Body   []byte
	Err    error
	Sends  int
}

// classifyRankedAnswer tells the status of a claim's answer, and whether it
// is final.
func classifyRankedAnswer(body []byte, err error) (status string, final bool) {
	if err != nil {
		switch nakamaRPCErrorCode(err) {
		case 3, 5, 7: // invalid argument, not found, permission denied
			return "refused", true
		case 9: // failed precondition: the set takes no more claims
			// A set that is no longer open answers a claim that did not
			// count with what became of it ("ranked set <id> is <status>:
			// ..."): past the claim window, a claim that waited is
			// answered so.
			var rpcErr *NakamaRPCError
			if errors.As(err, &rpcErr) {
				for _, status := range []string{"void", "disputed", "settled"} {
					if strings.Contains(rpcErr.Message, " is "+status+":") {
						return status, true
					}
				}
			}
			return "refused", true
		}
		// No answer, aborted, internal, unavailable, unauthenticated: send
		// again.
		return "error", false
	}
	var answer struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(body, &answer) != nil {
		return "error", false
	}
	switch answer.Status {
	case "settled", "disputed", "void":
		return answer.Status, true
	case "":
		return "error", false
	}
	return answer.Status, false
}

// rankedQueued is a claim of the queue.
type rankedQueued struct {
	rankedPendingClaim
	client *NakamaClient // the client the set was played with; nil after a restart
	next   time.Time
}

// rankedClaimQueue holds the claims that wait for a final answer, and sends
// them (one goroutine, one send at a time).
type rankedClaimQueue struct {
	mu       sync.Mutex
	path     string
	loaded   bool
	claims   []*rankedQueued
	wake     chan struct{}
	running  bool
	listener func(rankedAnswer)
	clients  map[string]*NakamaClient // clients made to send claims again, by server
	// For tests: the clock and the send.
	now  func() time.Time
	send func(c *rankedQueued) ([]byte, error)
}

var rankedClaims = &rankedClaimQueue{}

func (q *rankedClaimQueue) clock() time.Time {
	if q.now != nil {
		return q.now()
	}
	return time.Now()
}

func (q *rankedClaimQueue) filePath() string {
	if q.path != "" {
		return q.path
	}
	return filepath.Join(sys.baseDir, "save", "ranked_pending.json")
}

// setListener sets the function told of every answer (from the queue's
// goroutine).
func (q *rankedClaimQueue) setListener(fn func(rankedAnswer)) {
	q.mu.Lock()
	q.listener = fn
	q.mu.Unlock()
}

// loadLocked reads the file once (the claims of an earlier run of the game).
func (q *rankedClaimQueue) loadLocked() {
	if q.loaded {
		return
	}
	q.loaded = true
	path := q.filePath()
	claims, expired, err := readRankedPending(path, q.clock())
	if err != nil {
		log.Printf("Ranked claims: %v; the file is ignored", err)
		return
	}
	for _, c := range expired {
		log.Printf("Ranked set %s: the claim is a day old and is no longer sent", c.SetID)
	}
	for _, c := range claims {
		if q.findLocked(c.SetID, c.UserID) < 0 {
			q.claims = append(q.claims, &rankedQueued{rankedPendingClaim: c})
		}
	}
	if len(claims) > 0 {
		log.Printf("Ranked claims: %d from %s", len(claims), path)
	}
	if len(expired) > 0 {
		q.saveLocked()
	}
}

func (q *rankedClaimQueue) findLocked(setID, userID string) int {
	for i, c := range q.claims {
		if c.SetID == setID && c.UserID == userID {
			return i
		}
	}
	return -1
}

func (q *rankedClaimQueue) saveLocked() {
	out := make([]rankedPendingClaim, 0, len(q.claims))
	for _, c := range q.claims {
		out = append(out, c.rankedPendingClaim)
	}
	if err := writeRankedPending(q.filePath(), out); err != nil {
		log.Printf("Ranked claims: cannot write %s: %v", q.filePath(), err)
	}
}

// resume reads the claims of the file (once) and sends every claim now.
func (q *rankedClaimQueue) resume() {
	q.mu.Lock()
	q.loadLocked()
	q.dueNowLocked()
	q.mu.Unlock()
	q.kickWorker()
}

// reconnected sends every claim now (the game connected to a server again).
func (q *rankedClaimQueue) reconnected() {
	q.mu.Lock()
	if !q.loaded || len(q.claims) == 0 {
		q.mu.Unlock()
		return
	}
	q.dueNowLocked()
	q.mu.Unlock()
	q.kickWorker()
}

func (q *rankedClaimQueue) dueNowLocked() {
	now := q.clock()
	for _, c := range q.claims {
		c.next = now
	}
}

// add queues a claim and sends it now.
func (q *rankedClaimQueue) add(c rankedPendingClaim, client *NakamaClient) {
	q.mu.Lock()
	q.loadLocked()
	if c.Created == 0 {
		c.Created = q.clock().Unix()
	}
	entry := &rankedQueued{rankedPendingClaim: c, client: client, next: q.clock()}
	if i := q.findLocked(c.SetID, c.UserID); i >= 0 {
		q.claims[i] = entry
	} else {
		q.claims = append(q.claims, entry)
	}
	q.saveLocked()
	q.mu.Unlock()
	q.kickWorker()
}

// pending returns a copy of the queued claims and when each is sent next.
func (q *rankedClaimQueue) pending() []rankedQueued {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]rankedQueued, 0, len(q.claims))
	for _, c := range q.claims {
		out = append(out, rankedQueued{rankedPendingClaim: c.rankedPendingClaim, next: c.next})
	}
	return out
}

func (q *rankedClaimQueue) kickWorker() {
	q.mu.Lock()
	if q.wake == nil {
		q.wake = make(chan struct{}, 1)
	}
	if !q.running {
		q.running = true
		go q.work()
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// nextDueLocked returns the first claim due now, or how long until one is
// (0 when there are none).
func (q *rankedClaimQueue) nextDueLocked(now time.Time) (*rankedQueued, time.Duration) {
	var wait time.Duration
	for _, c := range q.claims {
		if !c.next.After(now) {
			return c, 0
		}
		if d := c.next.Sub(now); wait == 0 || d < wait {
			wait = d
		}
	}
	return nil, wait
}

func (q *rankedClaimQueue) work() {
	for {
		q.mu.Lock()
		now := q.clock()
		c, wait := q.nextDueLocked(now)
		if c == nil && wait == 0 {
			q.running = false
			q.mu.Unlock()
			return
		}
		wake := q.wake
		q.mu.Unlock()
		if c == nil {
			select {
			case <-time.After(wait):
			case <-wake:
			}
			continue
		}
		q.sendOne(c)
	}
}

// sendOne sends a claim once and decides what becomes of it.
func (q *rankedClaimQueue) sendOne(c *rankedQueued) {
	q.mu.Lock()
	now := q.clock()
	if c.expired(now) {
		q.removeLocked(c)
		q.saveLocked()
		listener := q.listener
		q.mu.Unlock()
		log.Printf("Ranked set %s: the claim is a day old and is no longer sent", c.SetID)
		if listener != nil {
			listener(rankedAnswer{SetID: c.SetID, Status: "expired", Final: true, Sends: c.Sends})
		}
		return
	}
	// Not sent again before the answer to this send.
	c.next = now.Add(time.Hour)
	send := q.send
	q.mu.Unlock()

	var body []byte
	var err error
	if send != nil {
		body, err = send(c)
	} else {
		body, err = q.rpc(c)
	}
	status, final := classifyRankedAnswer(body, err)

	q.mu.Lock()
	c.Sends++
	// (add may have put a newer claim of the set in its place meanwhile.)
	if i := q.findLocked(c.SetID, c.UserID); i >= 0 && q.claims[i] == c {
		if final {
			q.removeLocked(c)
		} else {
			c.next = q.clock().Add(rankedRetryDelay(c.Sends))
		}
		q.saveLocked()
	}
	listener := q.listener
	q.mu.Unlock()
	if err != nil {
		log.Printf("Ranked set %s: claim (send %d) %s: %v", c.SetID, c.Sends, status, err)
	} else {
		log.Printf("Ranked set %s: claim (send %d) %s: %s", c.SetID, c.Sends, status, body)
	}
	if !final {
		log.Printf("Ranked set %s: the claim is sent again in %v", c.SetID, rankedRetryDelay(c.Sends))
	}
	if listener != nil {
		listener(rankedAnswer{SetID: c.SetID, Status: status, Final: final, Body: body, Err: err, Sends: c.Sends})
	}
}

func (q *rankedClaimQueue) removeLocked(c *rankedQueued) {
	for i, x := range q.claims {
		if x == c {
			q.claims = append(q.claims[:i], q.claims[i+1:]...)
			return
		}
	}
}

// rpc sends the claim with the client the set was played with, or with a
// client of its own for the set's server and account.
func (q *rankedClaimQueue) rpc(c *rankedQueued) ([]byte, error) {
	client := c.client
	if client == nil || client.Session() == nil || client.Session().UserID != c.UserID {
		var err error
		if client, err = q.clientFor(c); err != nil {
			return nil, err
		}
	}
	return client.SubmitRankedSet(context.Background(), c.claim())
}

// clientFor returns a client of the claim's server signed in as its
// account: it uses the server's HTTP API alone (no realtime socket).
func (q *rankedClaimQueue) clientFor(c *rankedQueued) (*NakamaClient, error) {
	key := c.Server.key()
	q.mu.Lock()
	if q.clients == nil {
		q.clients = make(map[string]*NakamaClient)
	}
	client := q.clients[key]
	if client == nil {
		client = NewNakamaClient(NakamaConfig{
			Host:       c.Server.Host,
			Port:       c.Server.Port,
			ServerKey:  c.Server.ServerKey,
			UseTLS:     c.Server.SSL,
			DeviceID:   c.Server.DeviceID,
			RequestTTL: defaultNakamaRequestTTL,
		})
		q.clients[key] = client
	}
	q.mu.Unlock()
	if client.Session() == nil {
		if err := client.Authenticate(context.Background()); err != nil {
			return nil, err
		}
	}
	if s := client.Session(); s == nil || s.UserID != c.UserID {
		// Another account: the claim cannot be sent as the one that played.
		return nil, &NakamaRPCError{ID: "ikemen_submit_result", Code: 7,
			Message: "this game now signs in as another account than the one that played the set"}
	}
	return client, nil
}

// pendingServerOf describes a client's server and account for a claim.
func pendingServerOf(n *NakamaClient) rankedPendingServer {
	cfg := n.cfg
	s := rankedPendingServer{Host: cfg.Host, Port: cfg.Port, SSL: cfg.UseTLS, ServerKey: cfg.ServerKey, DeviceID: cfg.DeviceID}
	if data, err := os.ReadFile(filepath.Join(sys.baseDir, "save", "nakama_device_id")); err == nil &&
		strings.TrimSpace(string(data)) == cfg.DeviceID {
		// The installation's device id: read from its file when sent again.
		s.DeviceID = ""
	}
	return s
}

// luaPayload is the "ranked_result" event's payload for an answer: the
// server's answer (when it gave one), with set_id, status, final and sends.
func (a rankedAnswer) luaPayload() map[string]any {
	out := map[string]any{}
	if v, ok := rpcAnswer(a.Body).(map[string]any); ok {
		out = v
	}
	out["set_id"] = a.SetID
	out["status"] = a.Status
	out["final"] = a.Final
	out["sends"] = a.Sends
	return out
}
