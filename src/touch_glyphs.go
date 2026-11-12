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

// touchButtons are the Android app's on-screen buttons (its
// virtual_controller.xml), named as their glyph images are, with the pad
// input each one plays.
var touchButtons = []struct{ name, pad string }{
	{"a", "A"}, {"b", "B"}, {"c", "RT"},
	{"x", "X"}, {"y", "Y"}, {"z", "RB"},
	{"d", "LB"}, {"w", "LT"},
	{"start", "START"}, {"menu", "BACK"},
	{"up", "DP_U"}, {"down", "DP_D"}, {"left", "DP_L"}, {"right", "DP_R"},
}

// TouchControlsProperties is [Touch Controls]: for each game button, the
// [Glyphs] tokens that Android's on-screen buttons try, in order, to show it.
type TouchControlsProperties struct {
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
