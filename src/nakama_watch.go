package main

import (
	"encoding/json"
	"time"
)

// Watching and connection quality in lobbies.
//
// Publishing: the netplay host of a rollback match publishes the match's
// replay stream through the joined Nakama match (see rollback.go
// preMatchSetup); in a ranked session both players do. A lobby decides per
// match whether it is published, with which delay, and what the header tells
// spectators (NakamaReplayPublish).
//
// Direct round trip: while two players have their P2P path open, pings over
// it measure their round trip (p2pMux). In a lobby, the client reports it
// with the "link" command, so members see the direct value instead of the
// round trip to the server. The command also names the route the path runs
// over (direct or relayed, see NakamaP2P.RouteName), which the lobby shows
// and the server logs once per pairing and change (nakama/README.md,
// Connection telemetry); a ranked or quick-match session gets the same
// command when the route is first known and when it changes.

// rankedReplayDelaySeconds is the publication delay in a ranked session
// (setReplayPublish does not change it): the server keeps the stream for
// moderators and nobody watches it, so it only waits out rollbacks (GGPO goes
// back 8 frames at most), and a disconnect loses little of the match.
const rankedReplayDelaySeconds = 2

// NakamaReplayPublish decides how this client publishes the replay stream of
// its next rollback matches. The zero value publishes with the configured
// Netplay.Rollback.ReplayBroadcastDelay and no match description.
type NakamaReplayPublish struct {
	Disabled bool
	// DelaySeconds replaces ReplayBroadcastDelay when above 0.
	DelaySeconds int
	// Info goes to spectators in the header (ReplayStreamHeader.MatchInfo).
	Info json.RawMessage
}

func (n *NakamaClient) SetReplayPublish(publish NakamaReplayPublish) {
	n.mu.Lock()
	n.replayPublish = publish
	n.mu.Unlock()
}

func (n *NakamaClient) ReplayPublish() NakamaReplayPublish {
	n.mu.Lock()
	defer n.mu.Unlock()
	publish := n.replayPublish
	publish.Info = append(json.RawMessage(nil), n.replayPublish.Info...)
	return publish
}

// SetReplayInfo replaces only the match description of NakamaReplayPublish.
func (n *NakamaClient) SetReplayInfo(info json.RawMessage) {
	n.mu.Lock()
	n.replayPublish.Info = append(json.RawMessage(nil), info...)
	n.mu.Unlock()
}

// ConnectionType is "wired", "wifi", "mobile" or "" (unknown or not yet
// detected) for the current connection to the server.
func (n *NakamaClient) ConnectionType() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.connection
}

// inLobby reports whether the joined match is a lobby (it has sent a state).
func (n *NakamaClient) inLobby(matchID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.matchID == matchID && n.lobbyStateMatch == matchID && n.lobbyState != nil
}

const (
	// Pings answered before the first report.
	linkReportSamples = 3
	// Later reports: when the round trip moved by linkReportChange (and by a
	// fifth), at most every linkReportInterval. A change of route is
	// reported at once.
	linkReportChange   = 10 * time.Millisecond
	linkReportInterval = 5 * time.Second
)

// linkReport is what reportLink last sent (sent is false before the first).
type linkReport struct {
	sent     bool
	rtt      time.Duration
	route    string
	upgrades int
	at       time.Time
}

// linkReportDue decides whether reportLink sends a report now: rtt is the
// rounded round trip after samples pings, route and upgrades are
// NakamaP2P.RouteName and Upgrades. In a lobby the round trip is reported
// as it moves; elsewhere (a ranked or quick-match session) only the route
// is, so the first report and each change of route or upgrades.
func linkReportDue(last linkReport, rtt time.Duration, samples int, route string, upgrades int, inLobby bool, now time.Time) bool {
	if samples < linkReportSamples || route == "" {
		return false
	}
	if !last.sent || route != last.route || upgrades != last.upgrades {
		return true
	}
	if !inLobby {
		return false
	}
	change := rtt - last.rtt
	if change < 0 {
		change = -change
	}
	return now.Sub(last.at) >= linkReportInterval && change >= linkReportChange && change*5 >= last.rtt
}

// linkPayload is the body of the "link" command: the paired player, the
// round trip in ms, the route the connection runs over (NakamaP2P.RouteName:
// "direct-host", "direct-srflx", ..., "relay-udp" or "relay-ws") and how
// many times it changed route after the first.
func linkPayload(peer string, rtt time.Duration, route string, upgrades int) map[string]any {
	return map[string]any{
		"peer":     peer,
		"rtt":      int(rtt / time.Millisecond),
		"route":    route,
		"upgrades": upgrades,
	}
}

// p2pRoute returns the route and route changes of the current P2P session
// ("" and 0 without one).
func (n *NakamaClient) p2pRoute() (string, int) {
	n.mu.Lock()
	p := n.p2p
	n.mu.Unlock()
	if p == nil {
		return "", 0
	}
	return p.RouteName(), p.Upgrades()
}

// reportLink sends the match the round trip to the paired player and the
// route the P2P path p of matchID runs over, while it is open: a lobby keeps
// both for its members and logs the route of each pairing; a ranked or
// quick-match session logs the route (it takes the same "link" command on
// the lobby command op code). The route is sent again whenever it changes.
func (n *NakamaClient) reportLink(p *NakamaP2P, matchID string) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var last linkReport
	for {
		select {
		case <-p.closed:
			return
		case <-ticker.C:
		}
		if n.CurrentMatchID() != matchID {
			return
		}
		pairing, ok := n.Pairing()
		if !ok || pairing.PeerUserID == "" {
			continue
		}
		rtt, samples := p.RTT()
		rtt = rtt.Round(time.Millisecond)
		route, upgrades := p.RouteName(), p.Upgrades()
		now := time.Now()
		if !linkReportDue(last, rtt, samples, route, upgrades, n.inLobby(matchID), now) {
			continue
		}
		err := n.SendLobbyCommand("link", linkPayload(pairing.PeerUserID, rtt, route, upgrades))
		if err == nil {
			last = linkReport{sent: true, rtt: rtt, route: route, upgrades: upgrades, at: now}
		}
	}
}
