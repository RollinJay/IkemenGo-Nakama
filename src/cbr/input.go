package cbr

// InputBits is one frame of input, stored facing-relative: back and forward
// rather than left and right, so a case recorded facing one way replays
// correctly facing the other.
type InputBits uint16

const (
	InU InputBits = 1 << iota
	InD
	InB
	InF
	InA
	InBtnB
	InC
	InX
	InY
	InZ
	InS
	InBtnD
	InW
	InM
)

const (
	inDirMask InputBits = InU | InD | InB | InF
	inBtnMask InputBits = InA | InBtnB | InC | InX | InY | InZ | InS | InBtnD | InW | InM
)

// EngineButtons is the engine's per-frame button array, in the engine's
// order: U, D, L, R, a, b, c, x, y, z, s, d, w, m.
type EngineButtons = [14]bool

// FromEngine converts the engine's absolute input to facing-relative bits.
// facingRight is true when the character faces the +x direction.
func FromEngine(btn EngineButtons, facingRight bool) InputBits {
	var b InputBits
	if btn[0] {
		b |= InU
	}
	if btn[1] {
		b |= InD
	}
	left, right := btn[2], btn[3]
	if facingRight {
		if left {
			b |= InB
		}
		if right {
			b |= InF
		}
	} else {
		if left {
			b |= InF
		}
		if right {
			b |= InB
		}
	}
	for i := 4; i < 14; i++ {
		if btn[i] {
			b |= InA << uint(i-4)
		}
	}
	return b
}

// ToEngine converts facing-relative bits to the engine's absolute array for
// a character currently facing as given.
func ToEngine(b InputBits, facingRight bool) EngineButtons {
	var btn EngineButtons
	btn[0] = b&InU != 0
	btn[1] = b&InD != 0
	back, fwd := b&InB != 0, b&InF != 0
	if facingRight {
		btn[2], btn[3] = back, fwd
	} else {
		btn[2], btn[3] = fwd, back
	}
	for i := 4; i < 14; i++ {
		btn[i] = b&(InA<<uint(i-4)) != 0
	}
	return btn
}

// Dir returns the direction as a numpad digit (5 is neutral), with opposing
// directions cancelling.
func (b InputBits) Dir() uint8 {
	up, down := b&InU != 0, b&InD != 0
	back, fwd := b&InB != 0, b&InF != 0
	if up && down {
		up, down = false, false
	}
	if back && fwd {
		back, fwd = false, false
	}
	switch {
	case up && back:
		return 7
	case up && fwd:
		return 9
	case up:
		return 8
	case down && back:
		return 1
	case down && fwd:
		return 3
	case down:
		return 2
	case back:
		return 4
	case fwd:
		return 6
	}
	return 5
}

// Buttons returns only the button bits.
func (b InputBits) Buttons() InputBits { return b & inBtnMask }

// Directions returns only the direction bits.
func (b InputBits) Directions() InputBits { return b & inDirMask }

// Neutral reports whether no direction and no button is held.
func (b InputBits) Neutral() bool { return b == 0 }

// Presses returns the buttons newly pressed relative to the previous frame.
func (b InputBits) Presses(prev InputBits) InputBits {
	return b.Buttons() &^ prev.Buttons()
}

// dirBits turns a numpad direction back into direction bits.
func dirBits(d uint8) InputBits {
	switch d {
	case 1:
		return InD | InB
	case 2:
		return InD
	case 3:
		return InD | InF
	case 4:
		return InB
	case 6:
		return InF
	case 7:
		return InU | InB
	case 8:
		return InU
	case 9:
		return InU | InF
	}
	return 0
}
