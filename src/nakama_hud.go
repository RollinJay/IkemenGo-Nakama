package main

import (
	"strings"
	"unicode/utf8"
)

// Online names on the fight screen. During an online match, the fight
// screen shows each player's online name in place of the name of the
// character that player is playing as: the first name slot of each side,
// which holds the current fighter in Turns and the leader in Simul and Tag
// (a tag partner who comes in becomes the leader). The other name slots,
// Turns' list of the fighters still to come, and the versus and victory
// screens keep the characters' names, and characters keep their own names
// for their code (ModifyPlayer lifebarname, name triggers).
//
// The names are display state only. The Lua online flows set them
// (nakama.setMatchNames) and they are never part of the rollback state.
// They show only in netplay, in live replays (a watched lobby match) and in
// match replays, so names left set cannot reach an offline match.

// onlineMatchNameMax is the longest name drawn, in characters (the online
// name rules allow 16).
const onlineMatchNameMax = 16

// The names of the players who start on sides 1 and 2, and their user IDs
// when known.
var (
	onlineMatchNames [2]string
	onlineMatchUsers [2]string
)

// setOnlineMatchNames sets the names of the players on side 1 (P1) and side
// 2 (P2); an empty name leaves that side's character name. user1 and user2
// are their user IDs, or empty: with them, a ranked set that switches sides
// (RankedSet.PlayerForSide) moves each name with its player.
func setOnlineMatchNames(name1, name2, user1, user2 string) {
	onlineMatchNames = [2]string{cleanMatchName(name1), cleanMatchName(name2)}
	onlineMatchUsers = [2]string{user1, user2}
}

// onlineNameForSide returns the name set for the player on side (0 or 1)
// now, or "".
func onlineNameForSide(side int) string {
	if side < 0 || side > 1 {
		return ""
	}
	if sys.rankedSet.Active() && onlineMatchUsers[0] != "" && onlineMatchUsers[1] != "" {
		user := sys.rankedSet.PlayerForSide(side + 1)
		for i, u := range onlineMatchUsers {
			if u == user {
				return onlineMatchNames[i]
			}
		}
	}
	return onlineMatchNames[side]
}

// onlineUserForSide returns the user ID set for the player on side (0 or 1)
// now, or "".
func onlineUserForSide(side int) string {
	if side < 0 || side > 1 {
		return ""
	}
	if sys.rankedSet.Active() && onlineMatchUsers[0] != "" && onlineMatchUsers[1] != "" {
		return sys.rankedSet.PlayerForSide(side + 1)
	}
	return onlineMatchUsers[side]
}

// cleanMatchName keeps at most onlineMatchNameMax characters of name, without
// control characters or surrounding spaces.
func cleanMatchName(name string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(name) {
		if r == utf8.RuneError || r < 0x20 || (r >= 0x7f && r < 0xa0) {
			continue
		}
		if n == onlineMatchNameMax {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// onlineMatchSession reports whether names may show: a netplay match (not
// the rollback sync test of offline play), a live replay or a match replay
// (which plays like a live replay).
func onlineMatchSession() bool {
	if sys.netConnection != nil {
		return true
	}
	if sys.rollback.session != nil && !sys.rollback.session.syncTest {
		return true
	}
	return sys.replayFile != nil && sys.replayFile.liveBuffer != nil
}

// fightScreenNameFor returns the name the fight screen draws in a name slot
// with font f and bank: the online name of the player on side for the first
// slot, when one is set and f can draw all of it, and otherwise the
// character's lifebar name. The player chose the name with their own
// lifebar, which can differ from this one.
func fightScreenNameFor(side, slot, charpn int, f *Fnt, bank int32) string {
	if slot == 0 && onlineMatchSession() {
		if name := onlineNameForSide(side); name != "" && fontDrawsAll(f, bank, name) {
			return name
		}
	}
	return sys.cgi[charpn].lifebarname
}

// fontDrawsAll reports whether f has a glyph for every character of text; a
// nil font draws nothing, so it counts as drawing all.
func fontDrawsAll(f *Fnt, bank int32, text string) bool {
	if f == nil {
		return true
	}
	for _, r := range text {
		if !fontHasRune(f, bank, r) {
			return false
		}
	}
	return true
}

// runeChecker is a TrueType font that can tell whether it has a glyph (the
// renderers' font types, see nakama_hud_*.go).
type runeChecker interface {
	hasRune(r rune) bool
}

// fontHasRune reports whether f draws r with the given bank, as Fnt.DrawText
// and the TrueType renderer look glyphs up. A space is always drawn.
func fontHasRune(f *Fnt, bank int32, r rune) bool {
	if f == nil {
		return false
	}
	if r == ' ' {
		return true
	}
	if f.Type == "truetype" {
		if c, ok := f.ttf.(runeChecker); ok {
			return c.hasRune(r)
		}
		// A renderer that cannot tell: assume the font has it.
		return f.ttf != nil
	}
	bt := int32(0)
	if f.BankType == "sprite" {
		bt = bank
	}
	return f.images[bt][r] != nil
}

// lifebarNameFonts lists the fonts (with their banks) of the first name slot
// of both sides in every team-mode layout the fight screen defines.
func lifebarNameFonts(fs *FightScreen) []FSText {
	var out []FSText
	for layout := range fs.names {
		for side := 0; side < 2; side++ {
			if side >= len(fs.names[layout]) || fs.names[layout][side] == nil {
				continue
			}
			nm := fs.names[layout][side].name
			if nm.fnt == nil {
				continue
			}
			dup := false
			for _, o := range out {
				if o.fnt == nm.fnt && o.font[1] == nm.font[1] {
					dup = true
					break
				}
			}
			if !dup {
				out = append(out, nm)
			}
		}
	}
	return out
}

// lifebarNameMissing returns the characters of name, each once and in order,
// that one of the lifebar's name fonts cannot draw. Online names are chosen
// from the characters the lifebar can show.
func lifebarNameMissing(fs *FightScreen, name string) []rune {
	fonts := lifebarNameFonts(fs)
	var missing []rune
	seen := map[rune]bool{}
	for _, r := range name {
		if seen[r] {
			continue
		}
		seen[r] = true
		for _, f := range fonts {
			if !fontHasRune(f.fnt, f.font[1], r) {
				missing = append(missing, r)
				break
			}
		}
	}
	return missing
}
