package main

import "testing"

func TestNormalizeNakamaLobbyFormat(t *testing.T) {
	tests := map[string]string{
		"":                "queue",
		"queue":           "queue",
		"QUEUE":           "queue",
		"winner_stays":    "winner_stays_on",
		"winner_stays_on": "winner_stays_on",
		"round_robin":     "round_robin",
		"bracket":         "bracket",
		"unknown":         "queue",
	}
	for input, want := range tests {
		if got := normalizeNakamaLobbyFormat(input); got != want {
			t.Fatalf("format %q = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizeNakamaLobbyMaxGames(t *testing.T) {
	tests := map[int]int{-1: 3, 0: 3, 1: 1, 3: 3, 99: 99, 100: 99}
	for input, want := range tests {
		if got := normalizeNakamaLobbyMaxGames(input); got != want {
			t.Fatalf("max games %d = %d, want %d", input, got, want)
		}
	}
}
