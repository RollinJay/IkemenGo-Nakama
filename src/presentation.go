package main

// presentationGeometry holds render-only layout information for the optional
// widescreen border/presentation canvas. It must never be part of
// SystemStateVars because changing it cannot affect deterministic gameplay.
type presentationGeometry struct {
	canvas [4]int32 // physical presentation canvas, normally [0,0,W,H]
	game   [4]int32 // physical game viewport inside canvas
	origin [2]int32 // current top-left render origin in canvas coordinates
}

func (s *System) presentationEnabled() bool {
	if s == nil {
		return false
	}
	return s.cfg.Video.Border.Enabled && s.cfg.Video.Border.Width > 0 && s.cfg.Video.Border.Height > 0
}

func (s *System) presentationCanvasSize(gameW, gameH int32) (int32, int32) {
	if s.presentationEnabled() {
		return Max(1, s.cfg.Video.Border.Width), Max(1, s.cfg.Video.Border.Height)
	}
	return Max(1, gameW), Max(1, gameH)
}

// configurePresentation computes the physical canvas and game viewport. The
// game is centered by default; offsets are additive and intentionally allowed
// to move the viewport partially outside the canvas for custom layouts.
func (s *System) configurePresentation(gameW, gameH int32) {
	canvasW, canvasH := s.presentationCanvasSize(gameW, gameH)
	gw, gh := Max(1, gameW), Max(1, gameH)

	// Keep the requested game resolution intact when it fits. If a custom
	// border is smaller than the game, scale the physical viewport down while
	// preserving the game aspect so the render surface still fits the canvas.
	if gw > canvasW || gh > canvasH {
		scale := Min(float32(canvasW)/float32(gw), float32(canvasH)/float32(gh))
		gw = Max(1, int32(float32(gw)*scale))
		gh = Max(1, int32(float32(gh)*scale))
	}

	x := (canvasW-gw)/2 + s.cfg.Video.Border.GameOffsetX
	y := (canvasH-gh)/2 + s.cfg.Video.Border.GameOffsetY

	s.presentation.canvas = [4]int32{0, 0, canvasW, canvasH}
	s.presentation.game = [4]int32{x, y, gw, gh}
	s.presentation.origin = [2]int32{0, 0}
}

func (s *System) presentationGameRect() [4]int32 {
	if s == nil || s.presentation.game[2] <= 0 || s.presentation.game[3] <= 0 {
		return s.scrrect
	}
	return s.presentation.game
}

func (s *System) renderViewport() [4]int32 {
	return [4]int32{s.presentation.origin[0], s.presentation.origin[1], s.scrrect[2], s.scrrect[3]}
}

func (s *System) outerRenderState() drawAspectState {
	return aspectStateForSize(s.presentation.canvas[2], s.presentation.canvas[3])
}

type renderSurfaceState struct {
	scrrect [4]int32
	origin  [2]int32
	aspect  drawAspectState
}

func (s *System) captureRenderSurfaceState() renderSurfaceState {
	return renderSurfaceState{
		scrrect: s.scrrect,
		origin:  s.presentation.origin,
		aspect:  s.captureAspectState(),
	}
}

func (s *System) beginRenderSurface(rect [4]int32, aspect drawAspectState) {
	s.scrrect = [4]int32{0, 0, rect[2], rect[3]}
	s.presentation.origin = [2]int32{rect[0], rect[1]}
	s.restoreAspectState(aspect)
	if gfx != nil {
		gfx.SetViewport(rect[0], rect[1], rect[2], rect[3])
	}
	if gfxFont != nil {
		gfxFont.UpdateResolution(int(rect[2]), int(rect[3]))
	}
}

func (s *System) restoreRenderSurface(st renderSurfaceState) {
	s.scrrect = st.scrrect
	s.presentation.origin = st.origin
	s.restoreAspectState(st.aspect)
	if gfx != nil {
		viewport := [4]int32{st.origin[0], st.origin[1], st.scrrect[2], st.scrrect[3]}
		gfx.SetViewport(viewport[0], viewport[1], viewport[2], viewport[3])
	}
	if gfxFont != nil {
		gfxFont.UpdateResolution(int(st.scrrect[2]), int(st.scrrect[3]))
	}
}

func (s *System) borderGameX() int32 {
	return s.presentation.game[0]
}

func (s *System) borderGameY() int32 {
	return s.presentation.game[1]
}

func (s *System) borderGameWidth() int32 {
	return s.presentation.game[2]
}

func (s *System) borderGameHeight() int32 {
	return s.presentation.game[3]
}
