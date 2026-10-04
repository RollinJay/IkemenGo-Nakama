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

	// unexplained counts attack starts with no command active while the
	// slot is driven: the character's own AI changing states directly
	// underneath the CBR layer.
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
// entering attack states with no command active.
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
	ps.ct.onEvict = func(cc chainCase, tick int64) {
		if cc.rec != nil && ps.rec != nil {
			ps.rec.finalizeLoose(cc.rec, tick)
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
		}
		ps.lastFeat = f
		ps.lastSelf = me.Root
		ps.lastWorld = s.World
		ps.observed = true
	}
}

// watchDriven flags the character's own AI as direct state change when,
// while the slot is driven, the character starts attacks from control with
// no command active. The recorder does the same for recorded play; this
// covers driven play, which is never recorded.
func (e *Engine) watchDriven(ps *playerState, self *CharSnap, roundState int32) {
	prev := &ps.lastSelf
	if !ps.observed || !self.CmdKnown || roundState != 2 || ps.st.ChangeStateAI {
		return
	}
	if prev.Ctrl && !self.Ctrl && self.StateNo != prev.StateNo && self.MoveType == MtAttack &&
		!significant(self.Commands, e.motions[ps.char]) {
		ps.unexplained++
		if ps.unexplained >= e.p.ChangeStateFlagCount {
			ps.st.ChangeStateAI = true
		}
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
// their demonstrated result, played cases update their value. A failure
// caused by injected execution noise is kept out of the value.
func (e *Engine) finalizeChain(ps *playerState, rc *resolvedChain) {
	n := len(rc.ch.cases)
	r := e.p.reward(&rc.comp)
	dropped := rc.comp.Result == CompDropped || rc.comp.Result == CompPunished
	for i, cc := range rc.ch.cases {
		switch {
		case cc.rec != nil:
			if ps.rec != nil {
				ps.rec.finalizeCase(cc.rec, rc, i, n)
			}
		case cc.caseID != 0:
			c := ps.st.Get(cc.caseID)
			if c == nil {
				continue
			}
			if cc.perturbed {
				recordPerturbedDrop(c, int(cc.level), dropped)
				if dropped {
					continue
				}
			}
			cr := r * pow32(e.p.ComboGamma, n-1-i)
			e.p.applyOutcome(c, cr, ps.opp, rc.endTick)
			e.p.applyAction(ps.st.action(c), cr, ps.opp, true)
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
				n := len(rc.ch.cases)
				for j, cc := range rc.ch.cases {
					credit := shares[i] * pow32(e.p.ComboGamma, n-1-j)
					if credit == 0 {
						continue
					}
					switch {
					case cc.rec != nil:
						if !cc.rec.Removed && cc.rec.Source != SrcTrial {
							cc.rec.Outcome.ValueSum += credit
						}
					case cc.caseID != 0:
						if c := ps.st.Get(cc.caseID); c != nil {
							applyCredit(c, credit, ps.opp)
							e.p.applyAction(ps.st.action(c), credit, ps.opp, false)
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
			ps.drv.cur = nil
			ps.drv.cpCache = nil
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
