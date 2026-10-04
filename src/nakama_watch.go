package main

import (
	"encoding/json"
	"time"
)

// Watching and connection quality in lobbies.
//
// Publishing: the netplay host of a rollback match publishes the match's
// replay stream through the joined Nakama match (see rollback.go
// preMatchSetup). A lobby decides per match whether it is published, with
// which delay, and what the header tells spectators (NakamaReplayPublish).
//
// Direct round trip: while two players have their P2P path open, pings over
// it measure their round trip (p2pMux). In a lobby, the client reports it
// with the "link" command, so members see the direct value instead of the
// round trip to the server.

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
	// fifth), at most every linkReportInterval.
	linkReportChange   = 10 * time.Millisecond
	linkReportInterval = 5 * time.Second
)

// reportLink sends the lobby the round trip to the paired player while the
// P2P path p of matchID is open.
func (n *NakamaClient) reportLink(p *NakamaP2P, matchID string) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	reported := time.Duration(-1)
	var reportedAt time.Time
	for {
		select {
		case <-p.closed:
			return
		case <-ticker.C:
		}
		if n.CurrentMatchID() != matchID {
			return
		}
		if !n.inLobby(matchID) {
			continue
		}
		pairing, ok := n.Pairing()
		if !ok || pairing.PeerUserID == "" {
			continue
		}
		rtt, samples := p.RTT()
		if samples < linkReportSamples {
			continue
		}
		rtt = rtt.Round(time.Millisecond)
		if reported >= 0 {
			change := rtt - reported
			if change < 0 {
				change = -change
			}
			if time.Since(reportedAt) < linkReportInterval || change < linkReportChange || change*5 < reported {
				continue
			}
		}
		err := n.SendLobbyCommand("link", map[string]any{
			"peer": pairing.PeerUserID,
			"rtt":  int(rtt / time.Millisecond),
		})
		if err == nil {
			reported, reportedAt = rtt, time.Now()
		}
	}
}
