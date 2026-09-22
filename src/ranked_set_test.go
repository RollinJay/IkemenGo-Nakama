package main

import "testing"

func TestRankedSetBestOfAndSideSwitch(t *testing.T) {
	var r RankedSet
	r.Begin([2]string{"alice", "bob"}, RankedSetRules{BestOf: 4, SwitchSides: true})
	if r.BestOf() != 5 {
		t.Fatalf("BestOf = %d, want 5", r.BestOf())
	}
	if got := r.PlayerForSide(1); got != "alice" {
		t.Fatalf("side 1 = %q, want alice", got)
	}
	if complete := r.RecordMatch(1); complete {
		t.Fatal("set completed after first match")
	}
	if got := r.PlayerForSide(1); got != "bob" {
		t.Fatalf("side 1 after switch = %q, want bob", got)
	}
	if got := r.PlayerForSide(2); got != "alice" {
		t.Fatalf("side 2 after switch = %q, want alice", got)
	}
}

func TestRankedSetCompletesOnMajority(t *testing.T) {
	var r RankedSet
	r.Begin([2]string{"alice", "bob"}, RankedSetRules{BestOf: 3})
	if r.RecordMatch(1) {
		t.Fatal("completed too early")
	}
	if r.RecordMatch(1) != true {
		t.Fatal("expected completion at 2 wins")
	}
	if r.SetWinner() != 0 {
		t.Fatalf("winner = %d, want player 0", r.SetWinner())
	}
}

func TestRankedSetWinnerKeepsSelectionUsesPreviousMatchWinner(t *testing.T) {
	var r RankedSet
	r.Begin([2]string{"alice", "bob"}, RankedSetRules{BestOf: 3, WinnerKeepsSelection: true})
	r.SetSelectionToken(0, "alice-team")
	r.SetSelectionToken(1, "bob-team")
	if r.RecordMatch(1) {
		t.Fatal("set completed too early")
	}
	if !r.WinnerSelectionLocked(0) {
		t.Fatal("previous winner should be locked")
	}
	if r.WinnerSelectionLocked(1) {
		t.Fatal("previous loser should not be locked")
	}
	if !r.WinnerSelectionLocked(0) {
		t.Fatal("winner lock should remain until next match result")
	}
	if r.RecordMatch(2) {
		t.Fatal("set should not complete after one win each")
	}
	if r.WinnerSelectionLocked(0) {
		t.Fatal("old winner should no longer be locked")
	}
	if !r.WinnerSelectionLocked(1) {
		t.Fatal("new winner should be locked")
	}
}

func TestRankedSetResetClearsPreviousWinner(t *testing.T) {
	var r RankedSet
	r.Begin([2]string{"alice", "bob"}, RankedSetRules{BestOf: 3})
	if r.RecordMatch(1) {
		t.Fatal("set completed too early")
	}
	if r.LastWinner() != 0 {
		t.Fatalf("last winner = %d, want player 0", r.LastWinner())
	}
	r.Reset()
	if got := r.LastWinner(); got != -1 {
		t.Fatalf("last winner after reset = %d, want -1", got)
	}
	if got := r.SetWinner(); got != -1 {
		t.Fatalf("set winner after reset = %d, want -1", got)
	}
	if r.Active() {
		t.Fatal("set should be inactive after reset")
	}
}

func TestRankedSetForfeitAndNextSet(t *testing.T) {
	var r RankedSet
	r.Begin([2]string{"p1", "p2"}, RankedSetRules{BestOf: 3, SwitchSides: true})
	if r.StartNextSet() {
		t.Fatal("StartNextSet must require a completed set")
	}
	if !r.Forfeit(1) {
		t.Fatal("Forfeit should finish an active set")
	}
	if got := r.SetWinner(); got != 0 {
		t.Fatalf("forfeit winner = %d, want 0", got)
	}
	if r.MatchesPlayed() != 0 || r.Wins(0) != 0 || r.Wins(1) != 0 {
		t.Fatalf("forfeit should not fabricate a normal match result: matches=%d wins=%d/%d", r.MatchesPlayed(), r.Wins(0), r.Wins(1))
	}
	if !r.StartNextSet() {
		t.Fatal("StartNextSet should reset a completed set")
	}
	if r.SetWinner() != -1 || r.LastWinner() != -1 || r.MatchesPlayed() != 0 || r.Wins(0) != 0 || r.Wins(1) != 0 {
		t.Fatalf("next set did not reset score state: winner=%d last=%d matches=%d wins=%d/%d", r.SetWinner(), r.LastWinner(), r.MatchesPlayed(), r.Wins(0), r.Wins(1))
	}
	if r.SideForPlayer(0) != 1 || r.SideForPlayer(1) != 2 {
		t.Fatalf("next set did not reset side assignment: p1=%d p2=%d", r.SideForPlayer(0), r.SideForPlayer(1))
	}
}
