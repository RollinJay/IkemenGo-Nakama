package main

// RankedSetRules describes the match-series rules negotiated for a ranked set.
// It is presentation/session state and is intentionally outside rollback state.
type RankedSetRules struct {
	BestOf               int
	SwitchSides          bool
	WinnerKeepsSelection bool
}

// RankedSet tracks a best-of-N series between two player identities. Side
// assignment is separate from player identity so a set can swap physical sides
// between matches without changing who owns the set score.
type RankedSet struct {
	active      bool
	rules       RankedSetRules
	players     [2]string // stable player identity: [0]=player A, [1]=player B
	sidePlayers [2]int    // player index occupying side 1/2; values are 0/1
	wins        [2]int
	matches     int
	winner      int // player index, -1 until set completion
	lastWinner  int // player index who won the immediately preceding match
	selection   [2]string
}

func normalizeRankedBestOf(bestOf int) int {
	if bestOf < 1 {
		bestOf = 3
	}
	if bestOf%2 == 0 {
		bestOf++
	}
	if bestOf > 99 {
		bestOf = 99
	}
	return bestOf
}

func (r *RankedSet) Begin(players [2]string, rules RankedSetRules) {
	rules.BestOf = normalizeRankedBestOf(rules.BestOf)
	r.active = true
	r.rules = rules
	r.players = players
	r.sidePlayers = [2]int{0, 1}
	r.wins = [2]int{0, 0}
	r.matches = 0
	r.winner = -1
	r.lastWinner = -1
	r.selection = [2]string{"", ""}
}

func (r *RankedSet) Reset() {
	*r = RankedSet{}
	r.winner = -1
	r.lastWinner = -1
}

func (r *RankedSet) Active() bool { return r != nil && r.active }

func (r *RankedSet) Rules() RankedSetRules {
	if r == nil {
		return RankedSetRules{BestOf: 3}
	}
	return r.rules
}

func (r *RankedSet) BestOf() int { return normalizeRankedBestOf(r.rules.BestOf) }

func (r *RankedSet) MatchesPlayed() int {
	if r == nil {
		return 0
	}
	return r.matches
}

func (r *RankedSet) Wins(player int) int {
	if r == nil || player < 0 || player > 1 {
		return 0
	}
	return r.wins[player]
}

func (r *RankedSet) SetWinner() int {
	if r == nil {
		return -1
	}
	return r.winner
}

func (r *RankedSet) LastWinner() int {
	if r == nil {
		return -1
	}
	return r.lastWinner
}

// PlayerForSide returns the stable player identity currently occupying a game side.
func (r *RankedSet) PlayerForSide(side int) string {
	if r == nil || side < 1 || side > 2 || !r.active {
		return ""
	}
	return r.players[r.sidePlayers[side-1]]
}

// SideForPlayer returns the game side currently assigned to a player identity.
// It returns 0 when the player is not part of the active set.
func (r *RankedSet) SideForPlayer(player int) int {
	if r == nil || player < 0 || player > 1 || !r.active {
		return 0
	}
	for side, owner := range r.sidePlayers {
		if owner == player {
			return side + 1
		}
	}
	return 0
}

// RecordMatch records the winner of one match. winnerSide is the physical game
// side (1 or 2). It returns true when the series is now complete.
// Forfeit ends the active set in favor of the other stable player without
// changing the match score. The forfeiting player loses the set immediately;
// the result remains available through SetWinner() until Reset() or StartNextSet().
func (r *RankedSet) Forfeit(player int) bool {
	if r == nil || !r.active || player < 0 || player > 1 {
		return false
	}
	r.winner = 1 - player
	r.lastWinner = r.winner
	return true
}

// StartNextSet resets the score and side assignment while retaining the same
// two stable players and negotiated rules. It is used after a completed ranked
// set when both players elect to play another set.
func (r *RankedSet) StartNextSet() bool {
	if r == nil || !r.active || r.winner < 0 {
		return false
	}
	r.sidePlayers = [2]int{0, 1}
	r.wins = [2]int{0, 0}
	r.matches = 0
	r.winner = -1
	r.lastWinner = -1
	r.selection = [2]string{"", ""}
	return true
}

func (r *RankedSet) RecordMatch(winnerSide int) bool {
	if r == nil || !r.active || winnerSide < 1 || winnerSide > 2 {
		return false
	}
	player := r.sidePlayers[winnerSide-1]
	r.wins[player]++
	r.matches++
	r.lastWinner = player

	needed := r.BestOf()/2 + 1
	if r.wins[player] >= needed {
		r.winner = player
		return true
	}

	if r.rules.SwitchSides {
		r.sidePlayers[0], r.sidePlayers[1] = r.sidePlayers[1], r.sidePlayers[0]
	}
	return false
}

// SetSelectionToken records a character/team selection signature for a player.
// The signature deliberately excludes palettes so a winner can keep the same
// character/team while changing colors.
func (r *RankedSet) SetSelectionToken(player int, token string) {
	if r == nil || player < 0 || player > 1 {
		return
	}
	r.selection[player] = token
}

func (r *RankedSet) SelectionToken(player int) string {
	if r == nil || player < 0 || player > 1 {
		return ""
	}
	return r.selection[player]
}

// WinnerSelectionLocked reports whether a player's prior selection must be
// preserved. Only the previous match winner is subject to this rule.
func (r *RankedSet) WinnerSelectionLocked(player int) bool {
	return r != nil && r.active && r.rules.WinnerKeepsSelection && r.lastWinner == player
}
