//go:build android

package main

// hasRune reports whether the font has a glyph for r (see lifebarNameMissing).
func (f *Font_GLES32) hasRune(r rune) bool {
	return f.ttf != nil && f.ttf.Index(r) != 0
}
