package main

import (
	"os"
	"path/filepath"
	"testing"
)

// lifebarFixture writes a motif that lists two alternative lifebars next to
// its default one and points sys at it. It returns the three fight.def paths.
func lifebarFixture(t *testing.T) (base, alt1, alt2 string, write func(name, content string) string) {
	t.Helper()
	dir := t.TempDir()
	write = func(name, content string) string {
		path := filepath.ToSlash(filepath.Join(dir, name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	base = write("fight.def", `[Round]
start.waittime = 30
ctrl.time = 30
round.time = 0
round.default.displaytime = 60
fight.time = 0
fight.displaytime = 60
KO.displaytime = 60
slow.time = 60
over.waittime = 45
over.hittime = 10
over.wintime = 45
over.time = 210
win.time = 60
win.displaytime = 540
draw.displaytime = 540
fadeout.time = 30
pos = 0,0

[Time]
pos = 160,124
framespercount = 60
`)
	// The alternative differs in every gameplay value it can reach.
	alt1 = write("classic/fight.def", `[Round]
start.waittime = 99
ctrl.time = 77
round.time = 5
round.default.displaytime = 10
fight.time = 5
fight.displaytime = 11
KO.displaytime = 12
slow.time = 1
over.waittime = 2
over.hittime = 3
over.wintime = 4
over.time = 5
win.time = 6
win.displaytime = 7
draw.displaytime = 8
fadeout.time = 9
pos = 12,34

[Time]
pos = 160,132
framespercount = 30
`)
	alt2 = write("other/fight.def", "[Round]\nctrl.time = 1\n")
	motif := write("system.def", `[Files]
fight = fight.def
lifebar2 = other/fight.def
lifebar2.name = "Other"
lifebar1 = classic/fight.def   ; the 2022 one
lifebar1.name = "Classic (5.0)"
lifebar3 = missing/fight.def
`)
	savedDef, savedFight, savedChoice := sys.motif.Def, sys.motif.Files.Fight, sys.cfg.Config.Lifebar
	savedFS, savedFfx := sys.fightScreen, sys.ffx
	t.Cleanup(func() {
		sys.motif.Def, sys.motif.Files.Fight, sys.cfg.Config.Lifebar = savedDef, savedFight, savedChoice
		sys.fightScreen, sys.ffx = savedFS, savedFfx
	})
	sys.motif.Def, sys.motif.Files.Fight = motif, base
	return base, alt1, alt2, write
}

func TestLifebarChoicesFromMotif(t *testing.T) {
	base, alt1, alt2, _ := lifebarFixture(t)
	got := sys.lifebarChoices()
	want := []LifebarChoice{{"Default", base}, {"Classic (5.0)", alt1}, {"Other", alt2}}
	if len(got) != len(want) {
		t.Fatalf("choices = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Name != want[i].Name || !sameDefPath(got[i].Def, want[i].Def) {
			t.Fatalf("choice %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	for choice, wantDef := range map[int]string{0: base, 1: alt1, 2: alt2, 3: base, -1: base} {
		sys.cfg.Config.Lifebar = choice
		if def := selectedLifebarDef(base); !sameDefPath(def, wantDef) {
			t.Errorf("Lifebar=%d draws %q, want %q", choice, def, wantDef)
		}
	}
	// A fight screen that is not the motif's default is never replaced.
	sys.cfg.Config.Lifebar = 1
	if def := selectedLifebarDef(alt2); def != alt2 {
		t.Errorf("an explicit fight screen was replaced by %q", def)
	}
}

// Players who chose different lifebars must agree on the content fingerprint,
// and the fingerprint must still cover the alternatives' content.
func TestContentFingerprintIgnoresLifebarChoice(t *testing.T) {
	base, alt1, _, write := lifebarFixture(t)
	savedSelect := sys.motif.Files.Select
	t.Cleanup(func() { sys.motif.Files.Select = savedSelect })
	sys.motif.Files.Select = write("select.def", "[Characters]\nkfm\n")
	sys.ffx = map[string]*FightFx{}

	fingerprint := func(choice int, drawn string) string {
		sys.cfg.Config.Lifebar = choice
		sys.fightScreen.def, sys.fightScreen.baseDef = drawn, base
		return sys.currentContentFingerprint()
	}
	def := fingerprint(0, base)
	if classic := fingerprint(1, alt1); classic != def {
		t.Fatalf("the lifebar choice changed the fingerprint: %s vs %s", def, classic)
	}
	write("classic/fight.def", "[Round]\nctrl.time = 30\n; edited\n")
	if edited := fingerprint(0, base); edited == def {
		t.Fatal("an alternative lifebar's content is not covered by the fingerprint")
	}
}

// An alternative lifebar is drawn, but every value that affects the simulation
// comes from the default fight.def.
func TestLifebarGameplayFromDefault(t *testing.T) {
	base, alt1, _, _ := lifebarFixture(t)
	sys.ffx = map[string]*FightFx{}

	sys.cfg.Config.Lifebar = 0
	def, err := loadFightScreen(base)
	if err != nil {
		t.Fatal(err)
	}
	sys.cfg.Config.Lifebar = 1
	classic, err := loadFightScreen(base)
	if err != nil {
		t.Fatal(err)
	}
	if !sameDefPath(classic.def, alt1) || !sameDefPath(classic.gameplayDef(), base) {
		t.Fatalf("drawn %q / gameplay %q, want %q / %q", classic.def, classic.gameplayDef(), alt1, base)
	}
	// Drawing comes from the alternative.
	if classic.round.pos != [2]int32{12, 34} {
		t.Errorf("round pos = %v, want the alternative's 12,34", classic.round.pos)
	}

	a, b := def.round, classic.round
	ints := map[string][2]int32{
		"start.waittime":            {a.start_waittime, b.start_waittime},
		"ctrl.time":                 {a.ctrl_time, b.ctrl_time},
		"round.time":                {a.round_time, b.round_time},
		"round.sndtime":             {a.round_sndtime, b.round_sndtime},
		"fight.time":                {a.fight_time, b.fight_time},
		"slow.time":                 {a.slow_time, b.slow_time},
		"over.waittime":             {a.over_waittime, b.over_waittime},
		"over.hittime":              {a.over_hittime, b.over_hittime},
		"over.wintime":              {a.over_wintime, b.over_wintime},
		"over.time":                 {a.over_time, b.over_time},
		"win.time":                  {a.win_time, b.win_time},
		"round.default.displaytime": {a.round_default.displaytime, b.round_default.displaytime},
		"round1.displaytime":        {a.round[0].displaytime, b.round[0].displaytime},
		"fight.displaytime":         {a.fight.displaytime, b.fight.displaytime},
		"ko.displaytime":            {a.ko.displaytime, b.ko.displaytime},
		"win.displaytime":           {a.win[0].text[0].displaytime, b.win[0].text[0].displaytime},
		"draw.displaytime":          {a.drawgame.displaytime, b.drawgame.displaytime},
		"fadeout.time":              {a.fadeOut.time, b.fadeOut.time},
		"framespercount":            {def.time.framespercount, classic.time.framespercount},
		"overTime":                  {a.overTime(), b.overTime()},
	}
	for name, v := range ints {
		if v[0] != v[1] {
			t.Errorf("%s = %d with the alternative, %d with the default", name, v[1], v[0])
		}
	}
	if a.ctrl_time != 30 || def.time.framespercount != 60 {
		t.Fatalf("fixture not read: ctrl.time %d, framespercount %d", a.ctrl_time, def.time.framespercount)
	}

	// Reloading keeps following the choice.
	sys.fightScreen = *classic
	sys.cfg.Config.Lifebar = 0
	if err := sys.fightScreen.reload(); err != nil {
		t.Fatal(err)
	}
	if !sameDefPath(sys.fightScreen.def, base) {
		t.Errorf("reload after switching back draws %q", sys.fightScreen.def)
	}
}
