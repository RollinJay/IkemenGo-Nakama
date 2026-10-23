package cbr

import (
	"errors"
	"fmt"
	"math/rand"
)

// Engine is the engine-facing API. The adapter calls Frame once per logical
// frame with a complete snapshot, Input when resolving input for a player
// the CBR layer drives, and RoundEnd / MatchEnd at those boundaries. All
// calls come from the engine's main loop; the type is not safe for
// concurrent use.
type Engine struct {
	p   *Params
	dir string
	ids uint64
	rng *rand.Rand // seeds each driver

	stores  map[string]*Store
	motions map[string]map[string]Motion
	players []*playerState
}

type playerState struct {
	n    int
	char string
	opp  string
	st   *Store

	tr  *tracker
	ct  *chainTracker
	rec *recorder
	drv *driver

	log *matchLog // match-long recording state; survives per-round SetPlayer

	lastFeat  Feat
	lastSelf  CharSnap
	lastWorld WorldSnap
	observed  bool

	// unexplained counts attack starts with no command active during moves
	// the CBR layer played: the character's own AI changing states directly
	// while it was meant to be paused.
	unexplained int32
}

// New returns an engine storing data under dir. seed drives selection
// tie-breaks and execution noise.
func New(p *Params, dir string, seed int64) *Engine {
	if p == nil {
		p = DefaultParams()
	}
	return &Engine{p: p, dir: dir, rng: rand.New(rand.NewSource(seed)), stores: map[string]*Store{}, motions: map[string]map[string]Motion{}}
}

// SetMotions gives the canonical input sequence of each of a character's
// commands, by command name. The adapter synthesizes them from the
// character's command definitions; recording from assisted sources and
// hypothesis synthesis need them.
func (e *Engine) SetMotions(char string, m map[string]Motion) {
	e.motions[char] = m
	for _, ps := range e.players {
		if ps != nil && ps.char == char && ps.rec != nil {
			ps.rec.motions = m
		}
	}
}

// ChangeStateAI reports whether the character's own AI has been seen
// entering attack states with no command active (see Store.ChangeStateAI).
func (e *Engine) ChangeStateAI(char string) bool {
	st, err := e.Store(char)
	return err == nil && st.ChangeStateAI
}

// Params returns the engine's parameters.
func (e *Engine) Params() *Params { return e.p }

// Store returns the loaded store for a character, loading it on first use.
func (e *Engine) Store(char string) (*Store, error) {
	if s, ok := e.stores[char]; ok {
		return s, nil
	}
	s, err := LoadStore(LocalPath(e.dir, char), char)
	if err != nil {
		return nil, err
	}
	a, err := LoadAggregate(GlobalPath(e.dir, char))
	if err != nil {
		return nil, err
	}
	if a != nil {
		if err := s.ApplyGlobal(a); err != nil {
			return nil, err
		}
	}
	if s.maxID > e.ids {
		e.ids = s.maxID
	}
	e.stores[char] = s
	return s, nil
}

func (e *Engine) player(n int) *playerState {
	for len(e.players) <= n {
		e.players = append(e.players, nil)
	}
	return e.players[n]
}

// SetPlayer assigns a character to player slot n for the coming round. A
// slot keeps its match-long recording state while the character stays the
// same; a different character (a Turns change) closes the previous one's.
func (e *Engine) SetPlayer(n int, char, opponent string) error {
	if n < 0 {
		return errors.New("cbr: negative player")
	}
	st, err := e.Store(char)
	if err != nil {
		return err
	}
	old := e.player(n)
	st.touched = true
	ps := &playerState{n: n, char: char, opp: opponent, st: st}
	if old != nil {
		if old.char == char {
			ps.log = old.log
		} else {
			e.closeLog(old)
		}
	}
	ps.tr = newTracker(e.p, n)
	ps.ct = newChainTracker(e.p, n, &e.ids)
	ps.ct.onFinal = func(rc *resolvedChain) { e.finalizeChain(ps, rc) }
	// A case or probe that never joined an exchange resolves as neutral: a
	// recorded case and the base AI's record take the neutral result, and so
	// does a played case's record against the base AI, so the two stay
	// comparable. A played case's value is left as it was.
	ps.ct.onEvict = func(cc chainCase, tick int64) {
		neutral := e.p.Situation[CompNeutral]
		switch {
		case cc.probe:
			ps.st.addBaseline(e.p, cc.key, neutral)
		case cc.rec != nil:
			if ps.rec != nil {
				ps.rec.finalizeLoose(cc.rec, tick)
			}
		case cc.caseID != 0 && !cc.contested:
			if c := ps.st.Get(cc.caseID); c != nil {
				e.p.applyPlayed(ps.st.action(c), neutral)
			}
		}
	}
	e.players[n] = ps
	return nil
}

// Record starts recording player n from the given source.
func (e *Engine) Record(n int, src Source) error {
	ps := e.player(n)
	if ps == nil {
		return fmt.Errorf("cbr: player %d not set", n)
	}
	if ps.log != nil && ps.log.source != src {
		e.closeLog(ps)
	}
	if ps.log == nil {
		ps.log = &matchLog{source: src}
	}
	ps.rec = newRecorder(e.p, n, src, &e.ids, ps.ct, ps.st, e.motions[ps.char], ps.log)
	ps.rec.opponent = ps.opp
	return nil
}

// StopRecord stops recording player n. Cases already resolved are kept.
func (e *Engine) StopRecord(n int) {
	if ps := e.player(n); ps != nil && ps.rec != nil {
		e.collect(ps)
		e.closeLog(ps)
		ps.rec = nil
	}
}

// collect takes the recorder's resolved cases: human play is admitted at
// once, assisted play is staged in the match log until the match ends.
func (e *Engine) collect(ps *playerState) {
	cases := ps.rec.take()
	if ps.log != nil && ps.rec.source.assisted() {
		ps.log.staged = append(ps.log.staged, cases...)
		return
	}
	ps.st.Admit(e.p, cases)
}

// closeLog commits a slot's match log: staged cases and command-map
// observations unless the source was flagged for direct state change, and
// hypotheses either way.
func (e *Engine) closeLog(ps *playerState) {
	l := ps.log
	if l == nil {
		return
	}
	ps.log = nil
	if !l.flagged {
		for state, m := range l.pending {
			cs := ps.st.CmdStates[state]
			if cs == nil {
				cs = map[string]float32{}
				ps.st.CmdStates[state] = cs
			}
			for n, v := range m {
				cs[n] += v
			}
		}
		// With the whole match observed, an assisted case whose command no
		// longer wins for its state was credited to a coincidental command
		// (the engine AI's cheat picking an unrelated command that frame).
		motions := e.motions[ps.char]
		for _, c := range l.staged {
			if c.Command == "" {
				continue
			}
			if best, ok := ps.st.commandFor(c.MoveRef, l.active[c.ID], motions, c.ExecAttack, nil); !ok || best != c.Command {
				c.Removed = true
			}
		}
		ps.st.Admit(e.p, l.staged)
	}
	ps.st.Admit(e.p, l.hypo)
}

// Drive puts player n under CBR control at the given levels.
func (e *Engine) Drive(n int, lv Levels) error {
	ps := e.player(n)
	if ps == nil {
		return fmt.Errorf("cbr: player %d not set", n)
	}
	ps.drv = newDriver(e.p, n, lv, e.rng.Int63(), ps.st, ps.ct)
	ps.drv.opp = ps.opp
	return nil
}

// SetTakeoverMargin sets how much better than the base AI a move must have
// proved before the CBR layer takes over with it, for player n while it is
// driven (call it after Drive): level 1 Low, half of Params.TakeoverMargin;
// 2 Medium, TakeoverMargin; 3 High, twice it. Any other level is Medium.
func (e *Engine) SetTakeoverMargin(n, level int) {
	if ps := e.player(n); ps != nil && ps.drv != nil {
		ps.drv.margin = e.p.takeoverMargin(level)
	}
}

// TakeoverMargin returns player n's take-over margin, or 0 when the CBR
// layer does not drive it.
func (e *Engine) TakeoverMargin(n int) float32 {
	if ps := e.player(n); ps != nil && ps.drv != nil {
		return ps.drv.margin
	}
	return 0
}

// StopDrive returns player n to the engine's AI.
func (e *Engine) StopDrive(n int) {
	if ps := e.player(n); ps != nil {
		ps.drv = nil
	}
}

// Driving reports whether player n is under CBR control.
func (e *Engine) Driving(n int) bool {
	ps := e.player(n)
	return ps != nil && ps.drv != nil
}

// Frame consumes one complete logical frame. Call it after collisions for
// the frame have resolved. In netplay, never call it from inside the
// rollback simulation step: re-simulated frames would be observed twice.
// Learn from the match replay afterward instead.
func (e *Engine) Frame(s *Snapshot) {
	for _, ps := range e.players {
		if ps == nil || (ps.rec == nil && ps.drv == nil) {
			continue
		}
		me := s.Player(ps.n)
		if me == nil {
			// Not in the fight this frame (a Turns or Tag member waiting):
			// nothing to observe, and no input to give from stale state.
			ps.observed = false
			continue
		}
		ps.tr.observe(s)
		f := ps.tr.features(s)
		if ps.rec != nil {
			ps.rec.step(s, f)
		}
		ps.ct.step(s, ps.tr)
		if ps.drv != nil {
			ps.drv.observe(&me.Root)
			e.watchDriven(ps, &me.Root, s.World.RoundState)
			e.watchBase(ps, &me.Root, &s.World)
		}
		ps.lastFeat = f
		ps.lastSelf = me.Root
		ps.lastWorld = s.World
		ps.observed = true
	}
}

// watchDriven watches the moves the CBR layer plays. An attack started from
// control with no command active during one is the character's own AI
// changing states directly although it was paused: an older AI that a long
// command switched on, for instance, ignores the AILevel it reads. The move
// is dropped without counting against it (see driver.contest), and after
// ChangeStateFlagCount such starts the store notes the AI as direct state
// change. The recorder does the same for recorded play.
func (e *Engine) watchDriven(ps *playerState, self *CharSnap, roundState int32) {
	prev := &ps.lastSelf
	d := ps.drv
	if !ps.observed || !self.CmdKnown || roundState != 2 || !self.CBRDriven || d.cur == nil {
		return
	}
	if prev.Ctrl && !self.Ctrl && self.StateNo != prev.StateNo && self.MoveType == MtAttack &&
		self.StateNo != d.cur.MoveRef && !significant(self.Commands, e.motions[ps.char]) {
		d.contest()
		ps.unexplained++
		if ps.unexplained >= e.p.ChangeStateFlagCount {
			ps.st.ChangeStateAI = true
		}
	}
}

// watchBase adds a probe when, on a frame the CBR layer did not drive, the
// base AI starts an action from control, so its record covers what it does
// on its own as well as where the CBR layer left it in control.
func (e *Engine) watchBase(ps *playerState, self *CharSnap, w *WorldSnap) {
	prev := &ps.lastSelf
	if !e.p.BaseAI || !ps.observed || w.RoundState != 2 || self.CBRDriven {
		return
	}
	if prev.Ctrl && !self.Ctrl && self.StateNo != prev.StateNo && self.MoveType != MtHit {
		ps.drv.baseAction(&ps.lastFeat, prev, w.Tick)
	}
}

// Input returns the input for player n's next frame when the CBR layer
// drives it. ok is false when the layer declines, in which case the
// engine's own input source for that player stays in control. facingRight
// is the facing the engine will read the input with this frame (in Ikemen,
// the inverse of the character's forward/back flip), which can differ from
// the last observed facing when the character turns at the frame start.
func (e *Engine) Input(n int, facingRight bool) (btn EngineButtons, ok bool) {
	ps := e.player(n)
	if ps == nil || ps.drv == nil || !ps.observed {
		return btn, false
	}
	b, ok := ps.drv.input(&ps.lastWorld, &ps.lastSelf, &ps.lastFeat)
	if !ok {
		return btn, false
	}
	return ToEngine(b, facingRight), true
}

// finalizeChain credits a resolved chain to its cases: recorded cases take
// their demonstrated result, played cases update their value and their
// record against the base AI, and probes the base AI's record. A failure
// caused by injected execution noise is kept out of the value, and a play
// the character's own AI took over from counts for nothing.
func (e *Engine) finalizeChain(ps *playerState, rc *resolvedChain) {
	pos, after, n := rc.ch.split()
	r := e.p.reward(&rc.comp)
	dropped := rc.comp.Result == CompDropped || rc.comp.Result == CompPunished
	for i, cc := range rc.ch.cases {
		switch {
		case cc.probe:
			ps.st.addBaseline(e.p, cc.key, r*pow32(e.p.ComboGamma, after[i]))
		case cc.rec != nil:
			if ps.rec != nil {
				ps.rec.finalizeCase(cc.rec, rc, pos[i], n)
			}
		case cc.caseID != 0:
			c := ps.st.Get(cc.caseID)
			if c == nil || cc.contested {
				continue
			}
			if cc.perturbed {
				recordPerturbedDrop(c, int(cc.level), dropped)
				if dropped {
					continue
				}
			}
			cr := r * pow32(e.p.ComboGamma, n-1-pos[i])
			a := ps.st.action(c)
			e.p.applyOutcome(c, cr, ps.opp, rc.endTick)
			e.p.applyAction(a, cr, ps.opp, true)
			e.p.applyPlayed(a, r*pow32(e.p.ComboGamma, after[i]))
			ps.st.total++
			ps.st.evDirty = true
		}
	}
}

// RoundEnd resolves everything open, distributes the round's terminal
// reward, and admits the round's recorded cases. Call it once when the
// round's result is decided, with that frame's snapshot.
func (e *Engine) RoundEnd(s *Snapshot) {
	for _, ps := range e.players {
		if ps == nil || (ps.rec == nil && ps.drv == nil) {
			continue
		}
		me := s.Player(ps.n)
		chains := ps.ct.endRound(s)
		if me != nil {
			won := s.World.WinTeam >= 0 && int(s.World.WinTeam) == me.Root.TeamSide
			lost := s.World.WinTeam >= 0 && int(s.World.WinTeam) != me.Root.TeamSide
			shares := e.p.terminalShares(chains, won, lost)
			for i, rc := range chains {
				if shares[i] == 0 {
					continue
				}
				pos, after, n := rc.ch.split()
				for j, cc := range rc.ch.cases {
					credit := shares[i] * pow32(e.p.ComboGamma, n-1-pos[j])
					base := shares[i] * pow32(e.p.ComboGamma, after[j])
					switch {
					case cc.probe:
						ps.st.creditBaseline(cc.key, base)
					case cc.rec != nil:
						if !cc.rec.Removed && cc.rec.Source != SrcTrial {
							cc.rec.Outcome.ValueSum += credit
						}
					case cc.caseID != 0 && !cc.contested:
						if c := ps.st.Get(cc.caseID); c != nil {
							a := ps.st.action(c)
							applyCredit(c, credit, ps.opp)
							e.p.applyAction(a, credit, ps.opp, false)
							a.PSum += base
						}
					}
				}
			}
		}
		if ps.rec != nil {
			ps.rec.closeOpen(s.World.Tick)
			e.collect(ps)
			ps.rec.started = false
			ps.rec.skip = false
			ps.rec.recent = nil
			ps.rec.cmdFloor = 0
		}
		if ps.drv != nil {
			ps.drv.roundEnd()
		}
		ps.tr.reset()
		ps.observed = false
		ps.st.resetUsage()
		if ps.st.dirtyRemoved {
			ps.st.compact()
		}
	}
}

// MatchEnd admits staged cases, then prunes and saves every store used this
// match. Stores not used since the last MatchEnd are left alone.
func (e *Engine) MatchEnd() error {
	for _, ps := range e.players {
		if ps == nil {
			continue
		}
		if ps.rec != nil {
			e.collect(ps)
		}
		e.closeLog(ps)
	}
	var errs []error
	for char, st := range e.stores {
		if !st.touched {
			continue
		}
		st.Prune(e.p)
		if err := SaveStore(LocalPath(e.dir, char), st); err != nil {
			errs = append(errs, err)
			continue // stays touched, so the next match end retries
		}
		st.touched = false
	}
	e.players = nil
	return errors.Join(errs...)
}
