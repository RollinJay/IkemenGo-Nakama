package main

import "testing"

func TestPresentationGeometryCentersGame(t *testing.T) {
	oldCfg := sys.cfg.Video.Border
	oldGameW, oldGameH := sys.gameWidth, sys.gameHeight
	defer func() {
		sys.cfg.Video.Border = oldCfg
		sys.gameWidth, sys.gameHeight = oldGameW, oldGameH
	}()

	sys.cfg.Video.Border.Enabled = true
	sys.cfg.Video.Border.Width = 1920
	sys.cfg.Video.Border.Height = 1080
	sys.cfg.Video.Border.GameOffsetX = 0
	sys.cfg.Video.Border.GameOffsetY = 0
	sys.gameWidth, sys.gameHeight = 1440, 1080
	sys.configurePresentation(1440, 1080)
	got := sys.presentationGameRect()
	want := [4]int32{240, 0, 1440, 1080}
	if got != want {
		t.Fatalf("game rect = %v, want %v", got, want)
	}
}

func TestPresentationGeometryOffsetsGame(t *testing.T) {
	oldCfg := sys.cfg.Video.Border
	defer func() { sys.cfg.Video.Border = oldCfg }()

	sys.cfg.Video.Border.Enabled = true
	sys.cfg.Video.Border.Width = 1920
	sys.cfg.Video.Border.Height = 1080
	sys.cfg.Video.Border.GameOffsetX = -40
	sys.cfg.Video.Border.GameOffsetY = 20
	sys.configurePresentation(1440, 1080)
	got := sys.presentationGameRect()
	want := [4]int32{200, 20, 1440, 1080}
	if got != want {
		t.Fatalf("game rect = %v, want %v", got, want)
	}
}
