//go:build android || ios

package main

import (
	"fmt"
)

// touchGlyphsKey is what the app's button images were last written from, and
// touchGlyphsWait the frames until the next check.
// touchButtonsSent is the set of on-screen buttons the app last got, and
// touchControllersSent the number of real controllers, -1 before the first.
var (
	touchGlyphsKey       string
	touchGlyphsWait      int
	touchButtonsSent     = -1
	touchControllersSent = -1
)

// touchControlsUpdate keeps the app's on-screen buttons showing the glyphs of
// the game buttons they play, in touchGlyphsDir, and tells it which of them
// to show. It checks twice a second, since the screenpack, the options, the
// controller mapping and the touch controls' player can change.
func touchControlsUpdate() {
	if touchGlyphsWait > 0 {
		touchGlyphsWait--
		return
	}
	touchGlyphsWait = 30
	m := &sys.motif
	set := touchButtonsShown(sys.cfg.Input.TouchButtons, m.TouchControls.Buttons)
	if msg := touchButtonsMessage(set, touchStartSlot(m.TouchControls.Start.Slot, set), m.TouchControls.Layout); msg != touchButtonsSent {
		touchButtonsSent = msg
		sendToApp(appMsgTouchButtons, msg)
	}
	if appWantsControllerCount {
		touch := make([]bool, 0, len(input.controllers))
		for _, c := range input.controllers {
			if c != nil {
				touch = append(touch, isTouchControls(c))
			}
		}
		if n := realControllerCount(touch); n != touchControllersSent {
			touchControllersSent = n
			sendToApp(appMsgControllers, n)
		}
	}
	dir := touchGlyphsDir()
	if m.GlyphsSff == nil || m.GlyphsSff.filename == "" || dir == "" {
		return
	}
	tokens := touchGlyphTokens(touchControlsKeyConfig(), &m.TouchControls, func(token string) bool {
		g := m.Glyphs[token]
		return g != nil && m.GlyphsSff.GetSprite(uint16(g.Spr[0]), uint16(g.Spr[1])) != nil
	})
	sprites := make(map[string][2]int32, len(tokens))
	for name, token := range tokens {
		sprites[name] = m.Glyphs[token].Spr
	}
	key := fmt.Sprint(m.GlyphsSff.filename, sprites)
	if key == touchGlyphsKey {
		return
	}
	touchGlyphsKey = key
	if err := writeTouchGlyphs(dir, m.GlyphsSff.filename, sprites); err != nil {
		Logcat("Touch control glyphs: " + err.Error())
	}
	sendToApp(appMsgTouchGlyphs, len(sprites))
}

// touchControlsKeyConfig is the joystick mapping of the player the on-screen
// buttons play as: the one whose joystick is their controller slot, or the
// slot they will take at their first touch.
func touchControlsKeyConfig() KeyConfig {
	slot := -1
	for i, c := range input.controllers {
		if isTouchControls(c) {
			slot = i
			break
		}
	}
	if slot < 0 {
		slot = max(findFreeControllerSlot(), 0)
	}
	for _, kc := range sys.joystickConfig {
		if kc.Joy == slot {
			return kc
		}
	}
	if len(sys.joystickConfig) > 0 {
		return sys.joystickConfig[0]
	}
	return KeyConfig{}
}
