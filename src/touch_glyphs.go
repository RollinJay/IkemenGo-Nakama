package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// touchButtons are the Android and iOS apps' on-screen buttons (Android's
// virtual_controller.xml, iOS's TouchOverlayView.xib), named as their glyph
// images are, with the pad input each one plays. iOS's directional pad is a
// single disc, which shows no glyphs.
var touchButtons = []struct{ name, pad string }{
	{"a", "A"}, {"b", "B"}, {"c", "RT"},
	{"x", "X"}, {"y", "Y"}, {"z", "RB"},
	{"d", "LB"}, {"w", "LT"},
	{"start", "START"}, {"menu", "BACK"},
	{"up", "DP_U"}, {"down", "DP_D"}, {"left", "DP_L"}, {"right", "DP_R"},
}

// TouchControlsProperties is [Touch Controls]: for each game button, the
// [Glyphs] tokens that the Android and iOS on-screen buttons try, in order, to
// show it.
type TouchControlsProperties struct {
	// Buttons are the on-screen buttons shown (touchButtonTokens); empty
	// shows them all.
	Buttons []string `ini:"buttons"`
	// Start.Slot is a grid button (a, b, c, x, y, z, d or w) whose place
	// Start takes while that button is hidden; empty keeps Start beside Menu.
	Start struct {
		Slot string `ini:"slot"`
	} `ini:"start"`
	// Layout is how the buttons are arranged (touchLayouts): "arcade" (the
	// default) staggers the grid, "grid" puts each punch straight above its
	// kick, and straight, vewlix, arch and staggered lay the d-pad and the
	// eight attack buttons out as those arcade panels are.
	Layout string `ini:"layout"`
	// Panel places the attack buttons on an arcade panel's eight buttons,
	// left to right on its top and bottom rows (touchPanelMessage).
	Panel struct {
		Top    []string `ini:"top"`
		Bottom []string `ini:"bottom"`
	} `ini:"panel"`
	Glyph struct {
		A     []string `ini:"a"`
		B     []string `ini:"b"`
		C     []string `ini:"c"`
		X     []string `ini:"x"`
		Y     []string `ini:"y"`
		Z     []string `ini:"z"`
		S     []string `ini:"s"`
		D     []string `ini:"d"`
		W     []string `ini:"w"`
		M     []string `ini:"m"`
		Up    []string `ini:"up"`
		Down  []string `ini:"down"`
		Left  []string `ini:"left"`
		Right []string `ini:"right"`
	} `ini:"glyph"`
}

// touchButtonTokens name the on-screen buttons that [Touch Controls] buttons
// and config.ini's [Input] TouchButtons can show, by the letters they carry
// without a glyph, s and m being Start and Menu. Button i is bit i of the
// set the app gets (appMsgTouchButtons).
var touchButtonTokens = []string{"a", "b", "c", "x", "y", "z", "d", "w", "s", "m"}

// touchButtonsAll is the set of every on-screen button.
const touchButtonsAll = 1<<10 - 1

// parseTouchButtons reads a list of touchButtonTokens ("start" and "menu" may
// be spelled out, and "all" means every button) into a set. ok is false if
// the list names no button, which then shows none.
func parseTouchButtons(tokens []string) (set int, ok bool) {
	for _, tok := range tokens {
		switch tok = strings.ToLower(strings.TrimSpace(tok)); tok {
		case "all":
			return touchButtonsAll, true
		case "start":
			tok = "s"
		case "menu":
			tok = "m"
		}
		for i, name := range touchButtonTokens {
			if tok == name {
				set |= 1 << i
				ok = true
			}
		}
	}
	return set, ok
}

// touchButtonsShown is the set of on-screen buttons to show: those that the
// player's option (config.ini's [Input] TouchButtons, a comma-separated list)
// names, or if it names none, as when it is empty, the screenpack's
// [Touch Controls] buttons, or if those name none, all of them.
func touchButtonsShown(option string, screenpack []string) int {
	if set, ok := parseTouchButtons(strings.Split(option, ",")); ok {
		return set
	}
	if set, ok := parseTouchButtons(screenpack); ok {
		return set
	}
	return touchButtonsAll
}

// touchGridButtons is how many of touchButtonTokens, from the first, sit in
// the attack grid, where Start can take a hidden button's place.
const touchGridButtons = 8

// touchStartSlot is the index in touchButtonTokens of the grid button whose
// place Start takes, from [Touch Controls] start.slot, or -1 if it keeps its
// own: the slot must name a grid button that set hides, and Start must show.
func touchStartSlot(slot string, set int) int {
	slot = strings.ToLower(strings.TrimSpace(slot))
	for i, name := range touchButtonTokens[:touchGridButtons] {
		if slot == name && set&(1<<i) == 0 && set&(1<<8) != 0 {
			return i
		}
	}
	return -1
}

// touchLayouts name the on-screen layouts, layout i+1 being touchLayouts[i]
// (bits 16 to 19 of appMsgTouchButtons' parameter): classic, the app's own
// staggered grid ("arcade" in [Touch Controls]); grid, two straight rows;
// and four arcade panels from slagcoin.com's Japanese layouts, the d-pad
// where the stick is: straight (two straight rows of four), vewlix (the
// first two columns curving down to the stick), arch (the second column
// highest) and staggered (the top row 7 mm right of the bottom one, the
// first column lower). The app keeps the panels' coordinates
// (TouchPanels.java).
var touchLayouts = []string{"classic", "grid", "straight", "vewlix", "arch", "staggered"}

const (
	touchLayoutClassic = 1
	touchLayoutGridID  = 2
)

// touchLayoutID is the layout to use: the player's option (config.ini's
// [Input] TouchLayout) if it names one, or else the screenpack's
// [Touch Controls] layout, or else classic.
func touchLayoutID(option, screenpack string) int {
	for _, name := range []string{option, screenpack} {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "arcade" {
			name = "classic"
		}
		for i, l := range touchLayouts {
			if name == l {
				return i + 1
			}
		}
	}
	return touchLayoutClassic
}

// touchLayoutGrid is bit 14 of appMsgTouchButtons' parameter: the grid's
// buttons in two straight rows, x, y and z above a, b and c, with d and w in
// a fourth column and Start, when it is in the grid, apart on the right. An
// app that reads the layout from bits 16 to 19 ignores it.
const touchLayoutGrid = 1 << 14

// touchButtonsMessage is the parameter of appMsgTouchButtons: the set of
// buttons shown in bits 0 to 9, in bits 10 to 13 one more than the grid
// place Start takes (touchStartSlot), 0 for none, touchLayoutGrid for the
// grid layout, and the layout (touchLayoutID) in bits 16 to 19.
func touchButtonsMessage(set, startSlot, layout int) int {
	msg := set | (startSlot+1)<<10 | layout<<16
	if layout == touchLayoutGridID {
		msg |= touchLayoutGrid
	}
	return msg
}

// realControllerCount is how many of the connected controllers, each given by
// whether it is the touch controls' own (isTouchControls), are real ones. The
// iOS app hides its on-screen buttons while there is one (appMsgControllers).
func realControllerCount(touch []bool) int {
	n := 0
	for _, t := range touch {
		if !t {
			n++
		}
	}
	return n
}

// touchPanelDefault is where the attack buttons sit on a panel by default,
// top row then bottom row, left to right: the punches above the kicks, as in
// the classic layout.
var touchPanelDefault = [touchGridButtons]string{"x", "y", "z", "w", "a", "b", "c", "d"}

// touchPanelMessage is the parameter of appMsgTouchPanel: for each of a
// panel's eight places (top row left to right, then bottom row), in the 4
// bits from 4 times its number, one more than the index in
// touchButtonTokens of the grid button there. The places top and bottom
// name ([Touch Controls] panel.top and panel.bottom) come first; each grid
// button they leave out, or name twice, goes to the first place left free,
// those of touchPanelDefault first, so that every button has one.
func touchPanelMessage(top, bottom []string) int {
	var places [touchGridButtons]int // index+1, 0 for free
	placed := 0                      // bit i: button i has a place
	name := func(tok string) int {
		tok = strings.ToLower(strings.TrimSpace(tok))
		for i, n := range touchButtonTokens[:touchGridButtons] {
			if tok == n {
				return i
			}
		}
		return -1
	}
	for row, tokens := range [][]string{top, bottom} {
		for col, tok := range tokens {
			if col >= touchGridButtons/2 {
				break
			}
			if b := name(tok); b >= 0 && placed&(1<<b) == 0 {
				places[row*touchGridButtons/2+col] = b + 1
				placed |= 1 << b
			}
		}
	}
	for p, tok := range touchPanelDefault {
		b := name(tok)
		if placed&(1<<b) != 0 {
			continue
		}
		if places[p] != 0 {
			// Its own place is taken: the first free one.
			for p = 0; places[p] != 0; p++ {
			}
		}
		places[p] = b + 1
		placed |= 1 << b
	}
	msg := 0
	for p, v := range places {
		msg |= v << (4 * p)
	}
	return msg
}

// touchGlyphTokens picks, for each on-screen button, the glyph of the game
// button it plays in kc, the joystick mapping of the touch controls' player:
// the first token of that game button's list in tc for which has is true.
// Buttons without one are left out, and keep their letters.
func touchGlyphTokens(kc KeyConfig, tc *TouchControlsProperties, has func(token string) bool) map[string]string {
	g := &tc.Glyph
	buttons := []struct {
		input  int
		tokens []string
	}{
		{kc.dU, g.Up}, {kc.dD, g.Down}, {kc.dL, g.Left}, {kc.dR, g.Right},
		{kc.bA, g.A}, {kc.bB, g.B}, {kc.bC, g.C},
		{kc.bX, g.X}, {kc.bY, g.Y}, {kc.bZ, g.Z},
		{kc.bS, g.S}, {kc.bD, g.D}, {kc.bW, g.W}, {kc.bM, g.M},
	}
	byPad := make(map[string][]string, len(buttons))
	for _, b := range buttons {
		if pad, ok := ButtonToStringLUT[b.input]; ok {
			if _, taken := byPad[pad]; !taken {
				byPad[pad] = b.tokens
			}
		}
	}
	glyphs := make(map[string]string, len(touchButtons))
	for _, tb := range touchButtons {
		for _, token := range byPad[tb.pad] {
			if token = strings.TrimSpace(token); token != "" && has(token) {
				glyphs[tb.name] = token
				break
			}
		}
	}
	return glyphs
}

// writeTouchGlyphs writes each on-screen button's glyph sprite from sffFile as
// <dir>/<button>.png, and removes the images of buttons that have none.
func writeTouchGlyphs(dir, sffFile string, sprites map[string][2]int32) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	sff, err := readSffV2(sffFile)
	if err != nil {
		return err
	}
	var errs []error
	for _, tb := range touchButtons {
		path := filepath.Join(dir, tb.name+".png")
		spr, ok := sprites[tb.name]
		if ok {
			var img image.Image
			if img, err = sff.image(uint16(spr[0]), uint16(spr[1])); err == nil {
				var buf bytes.Buffer
				if err = png.Encode(&buf, img); err == nil {
					err = writeFileAtomic(path, buf.Bytes())
				}
			}
			if err == nil {
				continue
			}
			errs = append(errs, fmt.Errorf("%s: %w", tb.name, err))
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// writeFileAtomic writes a file that a reader on another thread never sees
// half written.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// sffV2 is an SFF v2 file read again for some of its sprites' pixels: the
// sprites the engine loads keep only their textures.
type sffV2 struct {
	data    *bytes.Reader
	sff     *Sff
	entries []sffV2Entry
}

type sffV2Entry struct {
	spr       *Sprite
	ofs, size uint32
	link      uint16
}

func readSffV2(filename string) (*sffV2, error) {
	f, err := OpenFile(filename)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		return nil, err
	}
	r := &sffV2{data: bytes.NewReader(raw), sff: newSff(SffTypeOther)}
	r.sff.filename = filename
	var lofs, tofs uint32
	if err := r.sff.header.Read(r.data, &lofs, &tofs); err != nil {
		return nil, err
	}
	if r.sff.header.Version[0] != 2 {
		return nil, fmt.Errorf("%s is SFF v%d, not v2", filename, r.sff.header.Version[0])
	}
	if _, err := r.sff.loadPalettes(r.data, lofs); err != nil {
		return nil, err
	}
	r.entries = make([]sffV2Entry, r.sff.header.NumberOfSprites)
	for i := range r.entries {
		// Sprite headers are 28 bytes each.
		if _, err := r.data.Seek(int64(r.sff.header.FirstSpriteHeaderOffset)+int64(i)*28, io.SeekStart); err != nil {
			return nil, err
		}
		e := &r.entries[i]
		e.spr = newSprite()
		if err := e.spr.readHeaderV2(r.data, &e.ofs, &e.size, lofs, tofs, &e.link); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// image decodes sprite group, number, following a link to the sprite whose
// pixels it shares. Indexed pixels take the sprite's own palette, with color 0
// transparent.
func (r *sffV2) image(group, number uint16) (image.Image, error) {
	found := -1
	for i, e := range r.entries {
		if e.spr.Group == group && e.spr.Number == number {
			found = i
			break
		}
	}
	if found < 0 {
		return nil, fmt.Errorf("no sprite %d,%d", group, number)
	}
	i := found
	for hops := 0; r.entries[i].size == 0; hops++ {
		if hops >= len(r.entries) || int(r.entries[i].link) >= len(r.entries) {
			return nil, fmt.Errorf("sprite %d,%d links nowhere", group, number)
		}
		i = int(r.entries[i].link)
	}
	e := r.entries[i]
	spr := e.spr
	w, h := int(spr.Size[0]), int(spr.Size[1])
	var px []byte
	switch format := -spr.rle; {
	case spr.rle == 0 && spr.coldepth == 8:
		px = make([]byte, e.size)
		if _, err := r.data.ReadAt(px, int64(e.ofs)); err != nil {
			return nil, err
		}
	case format >= 2 && format <= 4:
		// The data follows its uncompressed length.
		px = make([]byte, max(e.size, 4)-4)
		if _, err := r.data.ReadAt(px, int64(e.ofs)+4); err != nil {
			return nil, err
		}
		switch format {
		case 2:
			px = spr.Rle8Decode(px)
		case 3:
			px = spr.Rle5Decode(px)
		case 4:
			px = spr.Lz5Decode(px)
		}
	case format >= 10 && format <= 12:
		if _, err := r.data.Seek(int64(e.ofs)+4, io.SeekStart); err != nil {
			return nil, err
		}
		img, err := png.Decode(r.data)
		if err != nil {
			return nil, err
		}
		p, ok := img.(*image.Paletted)
		if format != 10 || !ok {
			return img, nil
		}
		// PNG8 takes the SFF's palette, not its own.
		b := p.Bounds()
		w, h = b.Dx(), b.Dy()
		px = make([]byte, 0, w*h)
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				px = append(px, p.ColorIndexAt(x, y))
			}
		}
	default:
		return nil, fmt.Errorf("sprite %d,%d: format %d, color depth %d not supported", group, number, format, spr.coldepth)
	}
	pal := r.sff.palList.Get(r.entries[found].spr.palidx)
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h && i < len(px); i++ {
		c := int(px[i])
		if c == 0 || c >= len(pal) {
			continue
		}
		col := pal[c]
		copy(img.Pix[i*4:], []byte{byte(col), byte(col >> 8), byte(col >> 16), byte(col >> 24)})
	}
	return img, nil
}
