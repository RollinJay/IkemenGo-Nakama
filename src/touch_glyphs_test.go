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
