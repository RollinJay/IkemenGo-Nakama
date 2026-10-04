package main

import (
	"strings"
	"testing"
)

// bitmapFont is a bitmap font with glyphs for chars in bank 0.
func bitmapFont(chars string) *Fnt {
	f := newFnt()
	f.images[0] = map[rune]*FntCharImage{}
	for _, r := range chars {
		f.images[0][r] = &FntCharImage{}
	}
	return f
}

// fakeTTF is a TrueType font that has the glyphs in has.
type fakeTTF struct{ has string }

func (fakeTTF) SetColor(red float32, green float32, blue float32, alpha float32) {}
func (fakeTTF) SetPalFX(spfx ShaderPalFX)                                        {}
func (fakeTTF) Width(scale float32, spacingXAdd float32, fs string, argv ...interface{}) float32 {
	return 0
}
func (fakeTTF) Printf(x, y float32, xscl, yscl float32, spacingXAdd float32, align int32, blend bool, window [4]int32,
	rxadd float32, rot Rotation, projectionMode int32, fLength float32, rcx, rcy float32,
	fs string, argv ...interface{}) error {
	return nil
}
func (fakeTTF) UpdateResolution(windowWidth int, windowHeight int) {}
func (f fakeTTF) hasRune(r rune) bool                              { return strings.ContainsRune(f.has, r) }

func nameSlot(f *Fnt, bank int32) *FightScreenName {
	nm := newFightScreenName()
	nm.name.fnt = f
	nm.name.font[1] = bank
	return nm
}

func missingText(fs *FightScreen, name string) string {
	return string(lifebarNameMissing(fs, name))
}

// Online names are chosen from the characters every name font of the
// lifebar can draw: the first name slot of both sides in every team mode.
func TestLifebarNameMissing(t *testing.T) {
	upper := bitmapFont("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-")
	mixed := bitmapFont("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789")
	fs := &FightScreen{}
	fs.names[0] = []*FightScreenName{nameSlot(upper, 0), nameSlot(upper, 0)}
	// Another team mode's P2 name font has lowercase letters but no '_'.
	fs.names[3] = []*FightScreenName{nameSlot(upper, 0), nameSlot(mixed, 0)}
	for name, want := range map[string]string{
		"KAI 2":  "",   // a space always draws
		"KAI_2":  "_",  // the Tag layout's font has no underscore
		"Kai":    "ai", // the single layout's font has capitals only
		"KAIKAI": "",
		"Kaa_":   "a_", // each missing character once, in order
	} {
		if got := missingText(fs, name); got != want {
			t.Errorf("missing(%q) = %q, want %q", name, got, want)
		}
	}
	// A sprite-bank font keeps glyphs per bank; the name's bank decides.
	spr := newFnt()
	spr.BankType = "sprite"
	spr.images[2] = map[rune]*FntCharImage{'Z': {}}
	fs = &FightScreen{}
	fs.names[0] = []*FightScreenName{nameSlot(spr, 2), nameSlot(spr, 2)}
	if got := missingText(fs, "ZZ"); got != "" {
		t.Errorf("sprite bank 2: missing %q", got)
	}
	fs.names[0][1] = nameSlot(spr, 1)
	if got := missingText(fs, "ZZ"); got != "Z" {
		t.Errorf("sprite bank 1: missing %q, want Z", got)
	}
	// A TrueType font answers for itself.
	ttf := newFnt()
	ttf.Type = "truetype"
	ttf.ttf = fakeTTF{has: "カイ"}
	fs = &FightScreen{}
	fs.names[0] = []*FightScreenName{nameSlot(ttf, 0), nameSlot(ttf, 0)}
	if got := missingText(fs, "カイX"); got != "X" {
		t.Errorf("truetype: missing %q, want X", got)
	}
	// No lifebar loaded: nothing can be checked, nothing is refused.
	if got := missingText(&FightScreen{}, "anything"); got != "" {
		t.Errorf("no fonts: missing %q", got)
	}
}

// The fight screen shows a player's name in the first name slot of their
// side, only in netplay and live replays, and only when that slot's font can
// draw it.
func TestFightScreenOnlineNames(t *testing.T) {
	savedNames := [3]string{sys.cgi[0].lifebarname, sys.cgi[1].lifebarname, sys.cgi[2].lifebarname}
	savedReplay, savedRanked := sys.replayFile, sys.rankedSet
	defer func() {
		sys.cgi[0].lifebarname, sys.cgi[1].lifebarname, sys.cgi[2].lifebarname = savedNames[0], savedNames[1], savedNames[2]
		sys.replayFile, sys.rankedSet = savedReplay, savedRanked
		setOnlineMatchNames("", "", "", "")
	}()
	sys.cgi[0].lifebarname, sys.cgi[1].lifebarname, sys.cgi[2].lifebarname = "Ryu", "Ken", "Guile"
	sys.replayFile = nil
	setOnlineMatchNames("  KAI\x07 ", "", "", "")
	upper := bitmapFont("ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	if got := fightScreenNameFor(0, 0, 0, upper, 0); got != "Ryu" {
		t.Fatalf("offline: %q, want the character's name", got)
	}
	sys.replayFile = &ReplayFile{liveBuffer: &NakamaReplayBuffer{}}
	for _, c := range []struct {
		side, slot, charpn int
		want               string
	}{
		{0, 0, 0, "KAI"},   // side 1's current character
		{0, 1, 2, "Guile"}, // side 1's partner keeps its name
		{1, 0, 1, "Ken"},   // no name set for side 2
	} {
		if got := fightScreenNameFor(c.side, c.slot, c.charpn, upper, 0); got != c.want {
			t.Errorf("side %d slot %d: %q, want %q", c.side, c.slot, got, c.want)
		}
	}
	// A slot whose font cannot draw the whole name (this game's lifebar
	// differs from the one the player chose the name with) keeps the
	// character's name.
	setOnlineMatchNames("Kai", "", "", "")
	if got := fightScreenNameFor(0, 0, 0, upper, 0); got != "Ryu" {
		t.Errorf("a font without lowercase: %q, want the character's name", got)
	}
	if got := fightScreenNameFor(0, 0, 0, bitmapFont("Kai"), 0); got != "Kai" {
		t.Errorf("a font with the name's glyphs: %q, want Kai", got)
	}
	// A ranked set that switches sides moves the names with the players.
	setOnlineMatchNames("HOST", "GUEST", "u-host", "u-guest")
	sys.rankedSet.Begin([2]string{"u-host", "u-guest"}, RankedSetRules{BestOf: 3, SwitchSides: true})
	if a, b := onlineNameForSide(0), onlineNameForSide(1); a != "HOST" || b != "GUEST" {
		t.Fatalf("first match: %q %q", a, b)
	}
	sys.rankedSet.RecordMatch(1)
	if a, b := onlineNameForSide(0), onlineNameForSide(1); a != "GUEST" || b != "HOST" {
		t.Fatalf("after the side switch: %q %q", a, b)
	}
}

func TestCleanMatchName(t *testing.T) {
	for in, want := range map[string]string{
		"  Kai  ":               "Kai",
		"Ka\x00i\u0085":         "Kai",
		"ABCDEFGHIJKLMNOPQRST":  "ABCDEFGHIJKLMNOP",
		"カイ・ブレード":               "カイ・ブレード",
		"":                      "",
		"sixteen chars ok":      "sixteen chars ok",
		"sixteen chars ok, not": "sixteen chars ok",
	} {
		if got := cleanMatchName(in); got != want {
			t.Errorf("cleanMatchName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Text typed by players is drawn as given: "\n" (two characters) starts a new
// line only in the screenpack's own texts.
func TestTextSpriteLiteral(t *testing.T) {
	ts := NewTextSprite()
	ts.text = `A\nB` + "\t"
	if got := ts.drawnText(); got != "A\nB    " {
		t.Errorf("screenpack text: %q", got)
	}
	ts.literal = true
	if got := ts.drawnText(); got != `A\nB`+"    " {
		t.Errorf("literal text: %q", got)
	}
	ts.Reset()
	if ts.literal {
		t.Errorf("Reset keeps the literal flag")
	}
}
