package cbr

import (
	"fmt"
	"math"
)

// The base AI.
//
// The character's own AI is the base, always: it plays whenever the CBR
// layer does not take over. (For a character without an AI of its own, the
// base is the engine's AI.) The CBR layer takes over only with a move that
// has proved better than the base AI in the same kind of situation, and only
// for that move: the adapter pauses the character's AI on the frames the CBR
// layer gives the input, and it plays again once the move is over.
//
// Better is measured with the chain reward the rest of the package learns
// from (see Params.reward): damage dealt, weighted by how the exchange ended,
// minus damage taken. The base AI's record is the baseline. Whenever the
// base AI keeps control at a decision point where the CBR layer had a move
// to offer, or starts an action of its own, a probe joins the chain tracker
// in place of a case. When the chain resolves, the probe takes the share of
// the chain's reward a played case in its place would take, and that share
// goes into the baseline for the probe's situation (see baselineKey). The
// base AI's moves are not cases and need not be: what matters is how it does
// from a situation, whatever it does there. Probes are recorded whether or
// not the base AI is recorded as well, and for an AI that changes states
// directly, whose recorded cases are discarded.
//
// The CBR layer's side of the comparison is what its own plays of a move
// achieved (ActionStat.PTrials and PSum), not the move's demonstrated value:
// a recording shows a move where the demonstrator chose it, which says
// little about how it does when the CBR layer chooses it. A move with no
// plays yet has proved nothing, so the CBR layer occasionally tries one
// whose demonstrations beat the base AI (see Params.TakeoverExplore), and
// only then does it start to take over with it.

// BaselineStat is the base AI's decayed record in one situation.
type BaselineStat struct {
	Trials   float32
	ValueSum float32
}

// Baseline keys come in three grains, marked in bits 20 and 21: a
// situation, the coarser situation it belongs to, and everything.
const (
	baseFine   uint64 = 1 << 20
	baseCoarse uint64 = 2 << 20
	baseAll    uint64 = 3 << 20
	baseGrain  uint64 = 3 << 20
	// The enemy's state type and the distance band, which the coarser
	// situation leaves out.
	baseNear uint64 = 0xff
)

// baselineKey identifies the situation of a decision for the baseline:
// actionKey's description of the situation without the move (the enemy's
// state type, the distance band, the exchange context and control), plus the
// AI level the base AI plays at, which its strength depends on.
func baselineKey(f *Feat, ctrl bool, aiLevel float32) uint64 {
	k := baseFine | uint64(f.Enemy.State&7)<<4 | distanceBand(f) |
		uint64(exchangeContext(f))<<8 | aiBand(aiLevel)<<13
	if ctrl {
		k |= 1 << 11
	}
	return k
}

// aiBand groups AI levels 1 to 3, 4 to 6 and 7 to 8.
func aiBand(level float32) uint64 {
	switch {
	case level > 6:
		return 2
	case level > 3:
		return 1
	}
	return 0
}

// coarseKey is the coarser situation of a situation: its exchange context,
// control and AI level band.
func coarseKey(k uint64) uint64 { return k&^(baseGrain|baseNear) | baseCoarse }

// decayed is the weight of n results in a row after recency decay, the most
// that n results can add up to.
func (p *Params) decayed(n float32) float32 {
	if p.Decay >= 1 || p.Decay <= 0 {
		return n
	}
	return float32((1 - math.Pow(float64(p.Decay), float64(n))) / (1 - float64(p.Decay)))
}

// addBaseline records one result of the base AI in a situation, into the
// situation, its coarser situation and the store-wide record.
func (s *Store) addBaseline(p *Params, key uint64, r float32) {
	for _, k := range [...]uint64{key, coarseKey(key), baseAll} {
		b := s.Baseline[k]
		if b == nil {
			b = &BaselineStat{}
			s.Baseline[k] = b
		}
		b.Trials = b.Trials*p.Decay + 1
		b.ValueSum = b.ValueSum*p.Decay + r
	}
}

// creditBaseline adds credit to a situation's record without counting a new
// result, for the round's terminal reward (see applyCredit).
func (s *Store) creditBaseline(key uint64, r float32) {
	for _, k := range [...]uint64{key, coarseKey(key), baseAll} {
		if b := s.Baseline[k]; b != nil {
			b.ValueSum += r
		}
	}
}

// baseline returns the base AI's mean result in a situation, shrunk toward
// the coarser records (see Params.BaselineShrink), and how many results the
// situation's own record holds.
func (s *Store) baseline(p *Params, key uint64) (mean, trials float32) {
	mean = p.BaselineDefault
	for _, k := range [...]uint64{baseAll, coarseKey(key), key} {
		if b := s.Baseline[k]; b != nil && p.BaselineShrink+b.Trials > 0 {
			mean = (p.BaselineShrink*mean + b.ValueSum) / (p.BaselineShrink + b.Trials)
		}
	}
	if b := s.Baseline[key]; b != nil {
		trials = b.Trials
	}
	return mean, trials
}

// applyPlayed records one result of the CBR layer's own play of an action.
func (p *Params) applyPlayed(a *ActionStat, r float32) {
	a.PTrials = a.PTrials*p.Decay + 1
	a.PSum = a.PSum*p.Decay + r
}

// takeoverMargin is the take-over margin of a setting: 1 Low, 2 Medium,
// 3 High; anything else is Medium.
func (p *Params) takeoverMargin(level int) float32 {
	switch level {
	case 1:
		return p.TakeoverMargin / 2
	case 3:
		return p.TakeoverMargin * 2
	}
	return p.TakeoverMargin
}

// gate is the comparison with the base AI for one choice.
type gate struct {
	key     uint64 // the situation (see baselineKey)
	explore bool   // an unproven move may be tried
	yield   bool   // leave even a move that proved better to the base AI
	// Set by the choice: held when moves matched but none may take over, so
	// the base AI keeps control; explored when the move chosen is an
	// unproven one tried by exploration.
	held, explored bool
}

// Rejection reasons of the gate, counted by chooseWhy.
const (
	// The move has not proved better than the base AI here.
	RejNotBetter = "not_better"
	// The move proved better, but this situation is left to the base AI to
	// keep its record current (see Params.BaseExplore).
	RejBaseExplore = "base_explore"
)

// gateFilter keeps the candidates that may take over from the base AI: the
// moves the CBR layer has played often enough whose results beat the base
// AI's by the margin. When none does and exploring is allowed, it keeps
// instead the unproven moves that may be tried (see explorable).
func (d *driver) gateFilter(picks []pick, g *gate, ambExp float64, rej func(string)) []pick {
	p, st := d.p, d.st
	b, n := st.baseline(p, g.key)
	// Counts are decayed, so n results in a row add up to decayed(n), less a
	// hair for rounding.
	margin := d.margin
	if n < p.decayed(p.BaselineMinTrials)-1e-3 {
		margin *= 2
	}
	minPlays := p.decayed(p.TakeoverMinPlays) - 1e-3
	var proven, untried []pick
	for _, pk := range picks {
		c := pk.c
		a := st.Actions[actionKey(c)]
		switch {
		case a != nil && a.PTrials >= minPlays:
			adv := (a.PSum - a.PTrials*b) / (p.TakeoverShrink + a.PTrials)
			if adv > 0 {
				adv *= float32(math.Pow(float64(d.completion(c)), ambExp))
			}
			if adv > margin {
				proven = append(proven, pk)
				continue
			}
		case g.explore && explorable(c, b):
			untried = append(untried, pk)
			continue
		}
		rej(RejNotBetter)
	}
	switch {
	case len(proven) > 0 && g.yield:
		for range proven {
			rej(RejBaseExplore)
		}
		g.held = true
		return nil
	case len(proven) > 0:
		return proven
	case len(untried) > 0:
		g.explored = true
		return untried
	}
	g.held = true
	return nil
}

// explorable reports whether an unproven move may be tried by exploration:
// the case's own result, as demonstrated (and as played, if it has been),
// beats the base AI's mean b. A hypothesis case has no result until played
// and is tried only this way.
func explorable(c *Case, b float32) bool {
	if c.Outcome.Trials > 0 {
		return c.Outcome.ValueSum/c.Outcome.Trials > b
	}
	return c.Source == SrcSynthesized
}

// decisionSit is a decision situation of the driver: a baseline situation
// in one state with or without control.
type decisionSit struct {
	key     uint64
	stateNo int32
	ctrl    bool
}

// enter notes the decision situation of a choice. In a new one, whether an
// unproven move may be tried and whether a proven one is left to the base
// AI are drawn once, so retries in the same situation do not draw again.
func (d *driver) enter(sit decisionSit) {
	if d.sitSeen && sit == d.sit {
		return
	}
	d.sit, d.sitSeen, d.sitProbed = sit, true, false
	p := d.p
	d.sitExplore = p.TakeoverExplore > 0 && d.explores < p.TakeoverExploreMax && d.rng.Float32() < p.TakeoverExplore
	d.sitYield = p.BaseExplore > 0 && d.rng.Float32() < p.BaseExplore
}

// decide makes a choice at a decision point. With the base AI on, only a
// move that proved better than the base AI here takes over (see
// gateFilter); when moves matched but none may, the base AI keeps control,
// and a probe follows how it does from here.
func (d *driver) decide(cur *Feat, self *CharSnap, tick int64) *Case {
	d.held = false
	if !d.p.BaseAI {
		c, _ := d.choose(cur, self, tick)
		return c
	}
	key := baselineKey(cur, self.Ctrl, self.AILevel)
	d.enter(decisionSit{key, self.StateNo, self.Ctrl})
	g := gate{key: key, explore: d.sitExplore, yield: d.sitYield}
	c, _ := d.chooseWhy(cur, self, tick, nil, &g)
	if c != nil && g.explored {
		d.explores++
		d.sitExplore = false
	}
	switch {
	case g.held:
		d.held = true
		d.probe(key, tick)
	case c != nil && d.sitProbed:
		// The CBR layer takes over in a situation it first left to the base
		// AI, before the situation changed (a cancel becomes possible a frame
		// later, say): the decision is the CBR layer's, not the base AI's.
		d.ct.withdraw(key, d.probeTick)
		d.sitProbed = false
	}
	return c
}

// probe adds a probe for the current decision situation, once.
func (d *driver) probe(key uint64, tick int64) {
	if d.sitProbed {
		return
	}
	d.sitProbed, d.probeTick = true, tick
	d.ct.addCase(chainCase{probe: true, key: key, startTick: tick})
}

// baseAction adds a probe for an action the base AI started on its own from
// the decision frame before (self, cur), unless the base AI's decision there
// already has one.
func (d *driver) baseAction(cur *Feat, self *CharSnap, tick int64) {
	key := baselineKey(cur, self.Ctrl, self.AILevel)
	if sit := (decisionSit{key, self.StateNo, self.Ctrl}); d.sitSeen && sit == d.sit {
		d.probe(key, tick)
		return
	}
	d.ct.addCase(chainCase{probe: true, key: key, startTick: tick})
}

// SituationString describes a baseline key, for inspection.
func SituationString(k uint64) string {
	if k&baseGrain == baseAll {
		return "all situations"
	}
	ai := [...]string{"AI level 1-3", "AI level 4-6", "AI level 7-8", "AI level ?"}[k>>13&3]
	s := contextString(k>>8&3) + ", " + ctrlString(k&(1<<11) != 0) + ", " + ai
	if k&baseGrain == baseCoarse {
		return s
	}
	return enemyString(k) + ", " + s
}

// ActionString describes an action key (see actionKey), for inspection.
func ActionString(k uint64) string {
	move := int32(uint32(k >> 16))
	what := fmt.Sprintf("move %d", move)
	if k&(1<<12) != 0 {
		what = "movement " + [...]string{"neutral", "forward", "back", "up", "down"}[min(max(int(-1-move), 0), 4)]
	}
	return what + ", " + enemyString(k) + ", " + contextString(k>>8&3) + ", " + ctrlString(k&(1<<11) != 0)
}

// enemyString describes the enemy's state type and distance in a key.
func enemyString(k uint64) string {
	st := ""
	switch StateType(k >> 4 & 7) {
	case StStand:
		st = "standing"
	case StCrouch:
		st = "crouching"
	case StAir:
		st = "airborne"
	case StLying:
		st = "lying"
	default:
		return "no enemy"
	}
	return st + " enemy " + bandString(k&15)
}

func contextString(ctx uint64) string {
	return [...]string{"enemy free", "enemy attacking", "enemy in hitstun", "enemy guarding"}[ctx&3]
}

func ctrlString(ctrl bool) string {
	if ctrl {
		return "with control"
	}
	return "mid-action"
}

func bandString(band uint64) string {
	switch b := int(band) - 4; {
	case b == 0:
		return "within 40 px"
	case band == 15:
		return "440 px or more ahead"
	case band == 0:
		return "160 px or more behind"
	case b > 0:
		return fmt.Sprintf("%d-%d px ahead", 40*b, 40*b+39)
	default:
		return fmt.Sprintf("%d-%d px behind", -40*b, -40*b+39)
	}
}
