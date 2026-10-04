package main

// Canonical inputs for a character's commands, for the CBR layer. AssertCommand
// and the engine AI's command cheat activate commands without their inputs,
// so the CBR layer records those actions with the command's canonical inputs
// instead (see package cbr, command.go). The inputs are generated from the
// compiled command steps in a few styles and kept only when the engine's own
// command matcher, run on a scratch copy of the command, completes the
// command with them.

import "github.com/ikemen-engine/Ikemen-GO/src/cbr"

// cbrMaxSynthSteps bounds the commands given a motion. Real moves rarely
// exceed eight steps (a 360 with a button); the long sequences some older AI
// uses as activation switches are left without one, so the CBR layer never
// inputs them.
const cbrMaxSynthSteps = 10

type cbrSynthStyle struct {
	gap      int  // frames each step's input is kept
	carryDir bool // keep the last direction held through button-only steps
	lead     int  // neutral frames before the motion
	// roll releases a direction by moving to the next step's direction, as
	// a person rolls a quarter circle, instead of through neutral.
	roll bool
}

var cbrSynthStyles = func() []cbrSynthStyle {
	var out []cbrSynthStyle
	for _, roll := range []bool{true, false} {
		for _, gap := range []int{1, 2} {
			for _, lead := range []int{0, 1} {
				for _, carry := range []bool{true, false} {
					out = append(out, cbrSynthStyle{gap, carry, lead, roll})
				}
			}
		}
	}
	return out
}()

func cbrKeyDir(k CommandKey) cbr.InputBits {
	switch k {
	case CK_U:
		return cbr.InU
	case CK_D:
		return cbr.InD
	case CK_B, CK_L:
		return cbr.InB
	case CK_F, CK_R:
		return cbr.InF
	case CK_UB, CK_UL:
		return cbr.InU | cbr.InB
	case CK_UF, CK_UR:
		return cbr.InU | cbr.InF
	case CK_DB, CK_DL:
		return cbr.InD | cbr.InB
	case CK_DF, CK_DR:
		return cbr.InD | cbr.InF
	}
	return 0
}

func cbrKeyButton(k CommandKey) cbr.InputBits {
	if k >= CK_a && k <= CK_m {
		return cbr.InA << uint(k-CK_a)
	}
	return 0
}

// cbrSynthFrames turns a command's steps into frames of facing-relative
// input (L and R read as back and forward, as for a character facing right).
func cbrSynthFrames(cmd *Command, st cbrSynthStyle) []cbr.InputBits {
	var out []cbr.InputBits
	var dir, holdDir, holdBtn, prevPress cbr.InputBits
	for i := 0; i < st.lead; i++ {
		out = append(out, 0)
	}
	for _, step := range cmd.steps {
		keys := step.keys
		if step.orLogic && len(keys) > 1 {
			keys = keys[:1]
		}
		var press, release, relDir, newDir cbr.InputBits
		dirSet := false
		charge, holdCharge := 0, 0
		for _, k := range keys {
			if k.key <= CK_N {
				d := cbrKeyDir(k.key)
				if k.tilde {
					relDir |= d
				} else {
					newDir, dirSet = d, true
					if k.slash {
						holdDir = d
						holdCharge = int(k.chargetime)
					}
				}
			} else {
				b := cbrKeyButton(k.key)
				if k.tilde {
					release |= b
				} else {
					press |= b
					if k.slash {
						holdBtn |= b
					}
				}
			}
			if k.tilde && int(k.chargetime) > charge {
				charge = int(k.chargetime)
			}
		}
		// A release needs the key held first: for its charge time, or one frame.
		if relDir != 0 || release != 0 {
			n := charge
			if n < 1 {
				n = 1
			}
			if relDir != 0 {
				dir = relDir
			}
			for j := 0; j < n; j++ {
				out = append(out, dir|holdBtn|release)
			}
			if st.roll && relDir != 0 && !dirSet && press == 0 && release == 0 {
				// The next step's direction releases this one.
				continue
			}
			if relDir != 0 {
				dir = holdDir
			}
		}
		// A held key with a charge time ("/30B") is held that long first.
		if holdCharge > 1 && dirSet {
			for j := 1; j < holdCharge; j++ {
				out = append(out, newDir|holdBtn)
			}
		}
		switch {
		case dirSet:
			// Pressing the direction already held needs a release first.
			if newDir == dir && newDir != 0 && holdCharge == 0 {
				out = append(out, holdBtn)
			}
			dir = newDir
		case !st.carryDir && press != 0:
			dir = holdDir
		}
		// The same button pressed on consecutive steps needs a release between.
		if press&prevPress != 0 {
			out = append(out, dir|holdBtn)
		}
		out = append(out, dir|holdBtn|press)
		prevPress = press
		for j := 1; j < st.gap; j++ {
			out = append(out, dir|holdBtn)
			prevPress = 0
		}
	}
	return out
}

// cbrVerifyMotion feeds frames to a scratch command list holding copies of
// the named command's definitions and returns the frame on which the
// command first completes.
func cbrVerifyMotion(cmds []Command, frames []cbr.InputBits) (int, bool) {
	if len(cmds) == 0 {
		return -1, false
	}
	cl := NewCommandList(NewInputBuffer())
	for _, c := range cmds {
		cc := c
		cc.completed = make([]bool, len(c.steps))
		cc.stepTimers = make([]int32, len(c.steps))
		cc.Clear(true)
		cl.Add(cc)
	}
	name := cmds[0].name
	for i, f := range frames {
		b := cbr.ToEngine(f, true)
		cl.Buffer.updateInputTime(b[0], b[1], b[2], b[3], b[2], b[3],
			b[4], b[5], b[6], b[7], b[8], b[9], b[10], b[11], b[12], b[13])
		cl.Step(false, false, false, false, 0)
		if cl.GetState(name) {
			return i, true
		}
	}
	return -1, false
}

// cbrMotions synthesizes and verifies a motion for each of a root's
// commands that can be input.
func cbrMotions(c *Char) map[string]cbr.Motion {
	return cbrMotionsFor(cbrCommandList(c))
}

func cbrMotionsFor(cl *CommandList) map[string]cbr.Motion {
	out := map[string]cbr.Motion{}
	if cl == nil {
		return out
	}
	for _, cmds := range cl.Commands {
		if len(cmds) == 0 {
			continue
		}
		name := cmds[0].name
	variants:
		for vi := range cmds {
			steps := len(cmds[vi].steps)
			if steps == 0 || steps > cbrMaxSynthSteps {
				continue
			}
			for _, st := range cbrSynthStyles {
				frames := cbrSynthFrames(&cmds[vi], st)
				if at, ok := cbrVerifyMotion(cmds, frames); ok {
					out[name] = cbr.Motion{Inputs: frames[:at+1], Steps: steps}
					break variants
				}
			}
		}
	}
	return out
}
