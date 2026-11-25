package main

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Lifebar choice
//
// A screenpack can offer alternative fight screens (lifebars) next to its
// default one, listed in the motif's [Files] section like the fonts:
//
//	fight = fight.def                   ; the default lifebar
//	lifebar0.name = "Modern"            ; optional label of the default
//	lifebar1 = classic/fight.def        ; an alternative
//	lifebar1.name = "Classic"           ; its label in the options menu
//
// Config.Lifebar picks one (0 = the default, N = lifebarN). The choice is
// a local, presentation-only preference: two players with different choices
// can play each other online. To keep that safe:
//
//   - everything that affects the simulation (the [Round] timings and
//     announcement display times, [Time] framespercount, the common FightFX the
//     fight screen loads, and where common files are searched) comes from the
//     default fight.def, whichever lifebar is drawn (applyLifebarGameplay,
//     FightScreen.gameplayDef);
//   - the netplay content fingerprint hashes the default fight.def and every
//     listed alternative, never just the chosen one.

// LifebarChoice is one selectable fight screen.
type LifebarChoice struct {
	Name string // label for the options menu
	Def  string // resolved fight.def path
}

const lifebarDefaultName = "Default"

// readDefSections returns the first occurrence of every section of an ini-like
// def file, keyed by lower-case section name (as ReadIniSection names them).
func readDefSections(path string) (map[string]IniSection, error) {
	str, err := LoadText(path)
	if err != nil {
		return nil, err
	}
	return parseDefSections(str), nil
}

func parseDefSections(str string) map[string]IniSection {
	out := map[string]IniSection{}
	lines := SplitAndTrim(NormalizeNewlines(str), "\n")
	i := 0
	for i < len(lines) {
		is, name, _ := ReadIniSection(lines, &i)
		if len(name) == 0 {
			break
		}
		name = strings.ToLower(strings.TrimSpace(name))
		if _, ok := out[name]; !ok {
			out[name] = is
		}
	}
	return out
}

func unquoteIniValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
	}
	return strings.TrimSpace(v)
}

// lifebarChoicesFrom lists the default fight screen (baseDef) followed by the
// alternatives the motif's [Files] section lists, in lifebarN order.
// Alternatives whose file cannot be found are left out.
func lifebarChoicesFrom(motifDef, baseDef string) []LifebarChoice {
	choices := []LifebarChoice{{Name: lifebarDefaultName, Def: baseDef}}
	if motifDef == "" {
		return choices
	}
	secs, err := readDefSections(motifDef)
	if err != nil {
		return choices
	}
	files := secs["files"]
	if files == nil {
		return choices
	}
	if n := unquoteIniValue(files["lifebar0.name"]); n != "" {
		choices[0].Name = n
	}
	type alt struct {
		n    int
		path string
	}
	var alts []alt
	for k, v := range files {
		if !strings.HasPrefix(k, "lifebar") {
			continue
		}
		n, err := strconv.Atoi(k[len("lifebar"):])
		if err != nil || n < 1 {
			continue // lifebarN.name, lifebar0 and malformed keys
		}
		alts = append(alts, alt{n, v})
	}
	sort.Slice(alts, func(i, j int) bool { return alts[i].n < alts[j].n })
	for _, a := range alts {
		v := filepath.ToSlash(strings.TrimSpace(a.path))
		if v == "" {
			continue
		}
		resolved := SearchFile(v, []string{motifDef, "", "data/"})
		if FileExist(resolved) == "" {
			LogMessage("Lifebar lifebar%v not found: %v", a.n, v)
			continue
		}
		name := unquoteIniValue(files["lifebar"+strconv.Itoa(a.n)+".name"])
		if name == "" {
			name = "Lifebar " + strconv.Itoa(a.n)
		}
		choices = append(choices, LifebarChoice{Name: name, Def: filepath.ToSlash(resolved)})
	}
	return choices
}

// motifFightDef resolves the motif's default fight.def, ignoring -fight.
func motifFightDef() string {
	v := sys.motif.Files.Fight
	if v == "" {
		if sys.motif.Def == "" {
			sys.motif.resolvePath()
		}
		v = readMotifFightFromDef(sys.motif.Def)
	}
	v = filepath.ToSlash(v)
	if v == "" {
		v = "fight.def"
	}
	resolved := SearchFile(v, []string{sys.motif.Def, "", "data/"})
	if FileExist(resolved) != "" {
		return filepath.ToSlash(resolved)
	}
	return v
}

// lifebarChoices lists the selectable fight screens of the current motif.
func (s *System) lifebarChoices() []LifebarChoice {
	return lifebarChoicesFrom(s.motif.Def, motifFightDef())
}

func sameDefPath(a, b string) bool {
	ca := strings.ToLower(filepath.Clean(filepath.ToSlash(a)))
	cb := strings.ToLower(filepath.Clean(filepath.ToSlash(b)))
	if ca == cb {
		return true
	}
	aa, errA := filepath.Abs(a)
	ab, errB := filepath.Abs(b)
	return errA == nil && errB == nil && strings.EqualFold(filepath.Clean(aa), filepath.Clean(ab))
}

// selectedLifebarDef returns the fight.def to draw for baseDef: the chosen
// alternative when baseDef is the motif's default fight screen and no -fight
// flag forces one, otherwise baseDef itself.
func selectedLifebarDef(baseDef string) string {
	n := sys.cfg.Config.Lifebar
	if n <= 0 {
		return baseDef
	}
	if _, ok := sys.cmdFlags["-fight"]; ok {
		return baseDef
	}
	if !sameDefPath(baseDef, motifFightDef()) {
		return baseDef
	}
	choices := lifebarChoicesFrom(sys.motif.Def, baseDef)
	if n >= len(choices) {
		return baseDef
	}
	return choices[n].Def
}

// gameplayDef is the fight.def whose gameplay values and search folder the
// match uses: the default one even when an alternative lifebar is drawn.
func (fs *FightScreen) gameplayDef() string {
	if fs.baseDef != "" {
		return fs.baseDef
	}
	return fs.def
}

// copyGameplayFrom copies the round values that drive the round state machine
// (and that characters can read through FightScreenVar/FightScreenState) from
// src, the default fight screen's [Round], leaving the drawing alone.
func (ro *FightScreenRound) copyGameplayFrom(src *FightScreenRound) {
	ro.start_waittime = src.start_waittime
	ro.round_time, ro.round_sndtime = src.round_time, src.round_sndtime
	ro.fight_time, ro.fight_sndtime = src.fight_time, src.fight_sndtime
	ro.ctrl_time = src.ctrl_time
	ro.ko_time, ro.ko_sndtime = src.ko_time, src.ko_sndtime
	ro.dko_time, ro.dko_sndtime, ro.dko_showdraw = src.dko_time, src.dko_sndtime, src.dko_showdraw
	ro.to_time, ro.to_sndtime = src.to_time, src.to_sndtime
	ro.slow_time, ro.slow_fadetime, ro.slow_speed = src.slow_time, src.slow_fadetime, src.slow_speed
	ro.over_waittime, ro.over_hittime = src.over_waittime, src.over_hittime
	ro.over_wintime, ro.over_forcewintime, ro.over_time = src.over_wintime, src.over_forcewintime, src.over_time
	ro.win_time, ro.win_sndtime = src.win_time, src.win_sndtime
	ro.shutter_time = src.shutter_time
	ro.callfight_time = src.callfight_time
	ro.clutch_threshold = src.clutch_threshold
	// Announcement display times decide when each round phase ends.
	for i := range ro.round {
		ro.round[i].displaytime = src.round[i].displaytime
	}
	ro.round_default.displaytime = src.round_default.displaytime
	ro.round_single.displaytime = src.round_single.displaytime
	ro.round_final.displaytime = src.round_final.displaytime
	ro.fight.displaytime = src.fight.displaytime
	ro.ko.displaytime = src.ko.displaytime
	ro.dko.displaytime = src.dko.displaytime
	ro.to.displaytime = src.to.displaytime
	ro.drawgame.displaytime = src.drawgame.displaytime
	for i := range ro.win {
		for side := range ro.win[i].text {
			ro.win[i].text[side].displaytime = src.win[i].text[side].displaytime
			ro.aiWin[i].text[side].displaytime = src.aiWin[i].text[side].displaytime
			ro.aiLose[i].text[side].displaytime = src.aiLose[i].text[side].displaytime
		}
	}
	// Fade times are part of the round/match end timing (overTime).
	if ro.fadeIn != nil && src.fadeIn != nil {
		ro.fadeIn.time = src.fadeIn.time
	}
	if ro.fadeOut != nil && src.fadeOut != nil {
		ro.fadeOut.time = src.fadeOut.time
	}
}

// applyLifebarGameplay overwrites fs's gameplay values with those of the
// default fight.def (base sections), so that an alternative lifebar only
// changes what is drawn and heard.
func applyLifebarGameplay(fs *FightScreen, base map[string]IniSection) {
	roundSec := base["round"]
	if roundSec == nil {
		roundSec = IniSection{}
	}
	// No font map: only the timings are wanted, and the default's fonts
	// aren't loaded (nil keeps readFSText from warning about them).
	ref := readFightScreenRound(roundSec, &Sff{}, NewAnimationTable(), &Snd{}, nil)
	if fs.round != nil {
		fs.round.copyGameplayFrom(ref)
	}
	if fs.time != nil {
		ft := int32(60)
		if ts := base["time"]; ts != nil {
			ts.ReadI32("framespercount", &ft)
		}
		fs.time.framespercount = ft
	}
}
