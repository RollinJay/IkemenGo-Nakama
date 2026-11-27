package main

import (
	"bytes"
	"encoding/binary"
	"image"
	imgcolor "image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/ini.v1"
)

// defaultTouchControls reads [Touch Controls] from the built-in motif as
// loadMotif does.
func defaultTouchControls(t *testing.T) TouchControlsProperties {
	t.Helper()
	f, err := LoadINIText(preprocessINIContent(NormalizeNewlines(string(defaultMotif))), ini.LoadOptions{
		InsensitiveSections: true, SkipUnrecognizableLines: true, PreserveSurroundedQuote: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sec, err := f.GetSection("Touch Controls")
	if err != nil {
		t.Fatal(err)
	}
	var m Motif
	for _, key := range sec.Keys() {
		if err := assignField(&m, parseQueryPath("touch_controls."+key.Name()), key.Value(), ""); err != nil {
			t.Fatalf("%s: %v", key.Name(), err)
		}
	}
	return m.TouchControls
}

// joystickMapping is a [Joystick_Px] mapping, in its order: up, down, left,
// right, a, b, c, x, y, z, start, d, w, menu.
func joystickMapping(tokens ...string) KeyConfig {
	if len(StringToButtonLUT) == 0 {
		initLUTs() // as realMain does
	}
	var v [14]int
	for i, tok := range tokens {
		v[i] = StringToButtonLUT[tok]
	}
	var kc KeyConfig
	kc.set(v)
	return kc
}

// defaultJoystick is defaultConfig.ini's [Joystick_P1].
var defaultJoystick = []string{"DP_U", "DP_D", "DP_L", "DP_R", "A", "B", "RT", "X", "Y", "RB", "START", "LB", "LT", "BACK"}

func TestTouchControlsDefaults(t *testing.T) {
	tc := defaultTouchControls(t)
	for _, c := range []struct {
		got, want []string
	}{
		{tc.Glyph.A, []string{"^LK", "^A"}},
		{tc.Glyph.X, []string{"^LP", "^X"}},
		{tc.Glyph.Z, []string{"^HP", "^Z"}},
		{tc.Glyph.S, []string{"^S"}},
		{tc.Glyph.Left, []string{"_B"}},
		{tc.Glyph.Right, []string{"_F"}},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestTouchGlyphsFollowTheButtonsAttacks(t *testing.T) {
	tc := defaultTouchControls(t)
	// A screenpack like HDBZ's, which draws the attacks and the letters.
	both := func(token string) bool { return token != "^W" }
	got := touchGlyphTokens(joystickMapping(defaultJoystick...), &tc, both)
	want := map[string]string{
		"a": "^LK", "b": "^MK", "c": "^HK", "x": "^LP", "y": "^MP", "z": "^HP",
		"d": "^D", "start": "^S", "menu": "^M",
		"up": "_U", "down": "_D", "left": "_B", "right": "_F",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("default mapping: got %v, want %v", got, want)
	}

	// Pad A and pad X swapped: the on-screen A plays x, a punch.
	swapped := append([]string(nil), defaultJoystick...)
	swapped[4], swapped[7] = "X", "A"
	got = touchGlyphTokens(joystickMapping(swapped...), &tc, both)
	if got["a"] != "^LP" || got["x"] != "^LK" {
		t.Errorf("swapped mapping: a = %q, x = %q, want ^LP, ^LK", got["a"], got["x"])
	}

	// A screenpack that draws letters only falls back to them.
	letters := func(token string) bool { return len(token) == 2 && token[0] == '^' }
	got = touchGlyphTokens(joystickMapping(defaultJoystick...), &tc, letters)
	if got["a"] != "^A" || got["z"] != "^Z" || got["start"] != "^S" {
		t.Errorf("letters only: a = %q, z = %q, start = %q, want ^A, ^Z, ^S", got["a"], got["z"], got["start"])
	}
	if _, ok := got["up"]; ok {
		t.Errorf("up got %q from a screenpack without direction glyphs", got["up"])
	}

	// A button whose input the mapping leaves out keeps its letter.
	unmapped := append([]string(nil), defaultJoystick...)
	unmapped[6] = "Not used" // c, which pad RT played
	got = touchGlyphTokens(joystickMapping(unmapped...), &tc, both)
	if token, ok := got["c"]; ok {
		t.Errorf("on-screen C plays nothing, got glyph %q", token)
	}
}

// testSffV2 builds an SFF v2.0.1.0 file holding img as sprite 60,0 (PNG32)
// and sprite 61,0 linked to it.
func testSffV2(t *testing.T, img image.Image) []byte {
	t.Helper()
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, img); err != nil {
		t.Fatal(err)
	}
	const headerSize, sprites = 64, 2
	ldata := uint32(headerSize + sprites*28)
	var b bytes.Buffer
	w := func(v any) { binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("ElecbyteSpr\x00")
	b.Write([]byte{0, 1, 0, 2}) // version 2.0.1.0, low byte first
	w(uint32(0))
	w([4]uint32{})
	w(uint32(headerSize)) // first sprite header
	w(uint32(sprites))
	w(uint32(0)) // first palette header
	w(uint32(0)) // palettes
	w(ldata)
	w(uint32(4 + pngData.Len()))
	w(ldata) // tdata, unused
	size := img.Bounds().Size()
	sprite := func(group, link uint16, length uint32) {
		w(group)
		w(uint16(0)) // number
		w([2]uint16{uint16(size.X), uint16(size.Y)})
		w([2]int16{})
		w(link)
		w(byte(12)) // PNG32
		w(byte(32))
		w(uint32(0)) // offset in ldata
		w(length)
		w(uint16(0)) // palette
		w(uint16(0)) // flags: ldata
	}
	sprite(60, 0, uint32(4+pngData.Len()))
	sprite(61, 0, 0)
	w(uint32(size.X * size.Y * 4))
	b.Write(pngData.Bytes())
	return b.Bytes()
}

func TestSffV2SpritesReadAgain(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	src.Set(1, 1, imgcolor.NRGBA{R: 200, G: 100, B: 50, A: 255})
	path := filepath.Join(t.TempDir(), "glyphs.sff")
	if err := os.WriteFile(path, testSffV2(t, src), 0o644); err != nil {
		t.Fatal(err)
	}
	sff, err := readSffV2(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []uint16{60, 61} {
		img, err := sff.image(group, 0)
		if err != nil {
			t.Fatalf("%d,0: %v", group, err)
		}
		if img.Bounds().Size() != src.Bounds().Size() {
			t.Fatalf("%d,0: size %v, want %v", group, img.Bounds().Size(), src.Bounds().Size())
		}
		if got := imgcolor.NRGBAModel.Convert(img.At(1, 1)); got != (imgcolor.NRGBA{R: 200, G: 100, B: 50, A: 255}) {
			t.Errorf("%d,0: pixel %v", group, got)
		}
	}
	if _, err := sff.image(62, 0); err == nil {
		t.Error("a missing sprite read without an error")
	}

	// An indexed sprite with its palette, from the repository's own font.
	font, err := readSffV2(filepath.Join("..", "font", "default-3x5.sff"))
	if err != nil {
		t.Fatal(err)
	}
	e := font.entries[0].spr
	img, err := font.image(e.Group, e.Number)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != int(e.Size[0]) || b.Dy() != int(e.Size[1]) {
		t.Errorf("font sprite %d,%d: %v, want %vx%v", e.Group, e.Number, b, e.Size[0], e.Size[1])
	}
	opaque := 0
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0 {
				opaque++
			}
		}
	}
	if opaque == 0 {
		t.Errorf("font sprite %d,%d decoded fully transparent", e.Group, e.Number)
	}
}

func TestWriteTouchGlyphs(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	sffPath := filepath.Join(t.TempDir(), "glyphs.sff")
	if err := os.WriteFile(sffPath, testSffV2(t, src), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "touch-glyphs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "z.png")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeTouchGlyphs(dir, sffPath, map[string][2]int32{"a": {60, 0}, "x": {61, 0}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "x"} {
		f, err := os.Open(filepath.Join(dir, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := png.DecodeConfig(f)
		f.Close()
		if err != nil || cfg.Width != 4 || cfg.Height != 4 {
			t.Errorf("%s.png: %+v, %v", name, cfg, err)
		}
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("z.png, whose button has no glyph now, stayed (%v)", err)
	}
}

func TestTouchButtonsParse(t *testing.T) {
	const a, b, c, x, y, z, d, w, s, m = 1, 2, 4, 8, 16, 32, 64, 128, 256, 512
	for _, tc := range []struct {
		tokens []string
		set    int
		ok     bool
	}{
		{[]string{"a", "b", "c", "x", "y", "z", "d", "w", "s", "m"}, touchButtonsAll, true},
		{[]string{"a", " B", "c ", "x", "y", "z", "s"}, a | b | c | x | y | z | s, true},
		{[]string{"start", "Menu"}, s | m, true},
		{[]string{"d", "w"}, d | w, true},
		{[]string{"ALL"}, touchButtonsAll, true},
		{[]string{"a", "up", "lp", "q"}, a, true},
		{[]string{"up", ""}, 0, false},
		{[]string{""}, 0, false},
		{nil, 0, false},
	} {
		set, ok := parseTouchButtons(tc.tokens)
		if set != tc.set || ok != tc.ok {
			t.Errorf("parseTouchButtons(%q) = %#x, %v; want %#x, %v", tc.tokens, set, ok, tc.set, tc.ok)
		}
	}
}

func TestTouchButtonsShown(t *testing.T) {
	noDW := touchButtonsAll &^ (1<<6 | 1<<7)
	hdbz := []string{"a", "b", "c", "x", "y", "z", "s", "m"}
	defaults := defaultTouchControls(t).Buttons
	if set, ok := parseTouchButtons(defaults); set != touchButtonsAll || !ok {
		t.Errorf("default motif buttons %q = %#x, want all", defaults, set)
	}
	for _, tc := range []struct {
		option     string
		screenpack []string
		want       int
	}{
		// The option's values in options.lua: Default, All, No D and W.
		{"", hdbz, noDW},
		{"", defaults, touchButtonsAll},
		{"a, b, c, x, y, z, d, w, s, m", hdbz, touchButtonsAll},
		{"a, b, c, x, y, z, s, m", defaults, noDW},
		{"all", hdbz, touchButtonsAll},
		{"  ", hdbz, noDW},
		// Neither names a button: all show.
		{"", nil, touchButtonsAll},
		{"nonsense", []string{""}, touchButtonsAll},
	} {
		if got := touchButtonsShown(tc.option, tc.screenpack); got != tc.want {
			t.Errorf("touchButtonsShown(%q, %q) = %#x, want %#x", tc.option, tc.screenpack, got, tc.want)
		}
	}
}

func TestTouchStartSlot(t *testing.T) {
	noDW := touchButtonsAll &^ (1<<6 | 1<<7)
	for _, tc := range []struct {
		slot string
		set  int
		want int
	}{
		{"", noDW, -1},
		{"d", noDW, 6},
		{" W ", noDW, 7},
		{"d", touchButtonsAll, -1},  // D shows: Start keeps its place
		{"d", noDW &^ (1 << 8), -1}, // Start hidden
		{"s", noDW, -1},             // not a grid button
		{"m", noDW &^ (1 << 9), -1}, // not a grid button
	} {
		if got := touchStartSlot(tc.slot, tc.set); got != tc.want {
			t.Errorf("touchStartSlot(%q, %#x) = %d, want %d", tc.slot, tc.set, got, tc.want)
		}
	}
	if got := touchButtonsMessage(noDW, 6, touchLayoutClassic); got != noDW|7<<10|1<<16 {
		t.Errorf("touchButtonsMessage = %#x", got)
	}
	if got := touchButtonsMessage(touchButtonsAll, -1, touchLayoutClassic); got != touchButtonsAll|1<<16 {
		t.Errorf("touchButtonsMessage without a slot = %#x", got)
	}
	if got := touchButtonsMessage(noDW, 6, touchLayoutGridID); got != noDW|7<<10|touchLayoutGrid|2<<16 {
		t.Errorf("touchButtonsMessage with the grid layout = %#x", got)
	}
	if l := defaultTouchControls(t).Layout; l != "arcade" {
		t.Errorf("default layout = %q, want arcade", l)
	}
	if s := defaultTouchControls(t).Start.Slot; s != "" {
		t.Errorf("default start.slot = %q, want empty", s)
	}
}

func TestRealControllerCount(t *testing.T) {
	for _, tc := range []struct {
		touch []bool
		want  int
	}{
		{nil, 0},
		{[]bool{true}, 0},         // only the on-screen buttons
		{[]bool{false}, 1},        // a real pad
		{[]bool{true, false}, 1},  // both
		{[]bool{false, false}, 2}, // two real pads
		{[]bool{true, true}, 0},   // never real, however many
		{[]bool{false, true, false}, 2},
	} {
		if got := realControllerCount(tc.touch); got != tc.want {
			t.Errorf("realControllerCount(%v) = %d, want %d", tc.touch, got, tc.want)
		}
	}
}

func TestAppMessages(t *testing.T) {
	seen := map[int]bool{}
	for _, c := range []int{appMsgTextEntry, appMsgTouchGlyphs, appMsgTouchButtons, appMsgControllers, appMsgTouchEdit, appMsgTouchPanel} {
		// SDL's Android activity leaves commands from 0x8000 to apps.
		if c < 0x8000 || seen[c] {
			t.Errorf("app message %#x is SDL's or taken", c)
		}
		seen[c] = true
	}
	// The iOS app reads these numbers (third_party/ikemen-ios TouchControls.h).
	if appMsgTextEntry != 0x8001 || appMsgTouchGlyphs != 0x8002 || appMsgTouchButtons != 0x8003 || appMsgControllers != 0x8004 ||
		appMsgTouchEdit != 0x8005 || appMsgTouchPanel != 0x8006 {
		t.Error("app message numbers changed: change the apps' too")
	}
}

func TestTouchLayoutID(t *testing.T) {
	for _, tc := range []struct {
		option, screenpack string
		want               int
	}{
		{"", "", touchLayoutClassic},
		{"", "arcade", touchLayoutClassic},
		{"", " Grid ", touchLayoutGridID},
		{"", "vewlix", 4},
		{"straight", "grid", 3},
		{"Staggered", "", 6},
		{"arch", "vewlix", 5},
		{"classic", "vewlix", touchLayoutClassic},
		{"nonsense", "vewlix", 4}, // an option the game doesn't know
		{"", "nonsense", touchLayoutClassic},
	} {
		if got := touchLayoutID(tc.option, tc.screenpack); got != tc.want {
			t.Errorf("touchLayoutID(%q, %q) = %d, want %d", tc.option, tc.screenpack, got, tc.want)
		}
	}
	// The app numbers them alike (TouchPanels.java).
	want := []string{"classic", "grid", "straight", "vewlix", "arch", "staggered"}
	if !reflect.DeepEqual(touchLayouts, want) {
		t.Errorf("touchLayouts = %q, want %q", touchLayouts, want)
	}
	if got := touchButtonsMessage(touchButtonsAll, -1, 4); got>>16&0xF != 4 || got&touchLayoutGrid != 0 {
		t.Errorf("touchButtonsMessage for vewlix = %#x", got)
	}
}

// panelPlaces reads a touchPanelMessage back as the button names in each
// place, "" for a free one.
func panelPlaces(msg int) []string {
	var places []string
	for p := 0; p < touchGridButtons; p++ {
		v := msg >> (4 * p) & 0xF
		if v == 0 {
			places = append(places, "")
		} else {
			places = append(places, touchButtonTokens[v-1])
		}
	}
	return places
}

func TestTouchPanelMessage(t *testing.T) {
	tc := defaultTouchControls(t)
	if !reflect.DeepEqual(tc.Panel.Top, []string{"x", "y", "z", "w"}) || !reflect.DeepEqual(tc.Panel.Bottom, []string{"a", "b", "c", "d"}) {
		t.Errorf("default panel = %q / %q", tc.Panel.Top, tc.Panel.Bottom)
	}
	for _, c := range []struct {
		top, bottom []string
		want        []string
	}{
		{nil, nil, []string{"x", "y", "z", "w", "a", "b", "c", "d"}},
		{tc.Panel.Top, tc.Panel.Bottom, []string{"x", "y", "z", "w", "a", "b", "c", "d"}},
		// HDBZ: the punches and D (where Start goes) on top, the kicks below.
		{[]string{"x", "y", "z", "d"}, []string{"a", "b", "c", "w"}, []string{"x", "y", "z", "d", "a", "b", "c", "w"}},
		// Only the top row: the others go to the first free places.
		{[]string{" A ", "B", "C", "D"}, nil, []string{"a", "b", "c", "d", "x", "y", "z", "w"}},
		// Twice, unknown and a fifth: ignored, their places left free for
		// the others (w's own place being y's).
		{[]string{"x", "x", "q", "y", "z"}, []string{"a"}, []string{"x", "w", "z", "y", "a", "b", "c", "d"}},
	} {
		got := panelPlaces(touchPanelMessage(c.top, c.bottom))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("touchPanelMessage(%q, %q) = %q, want %q", c.top, c.bottom, got, c.want)
		}
	}
	// Every place filled, the eighth too (its nibble's high bit is bit 31).
	if msg := touchPanelMessage(nil, []string{"", "", "", "w"}); msg>>28&0xF != 8 {
		t.Errorf("w in the last place = %#x", msg)
	}
}
