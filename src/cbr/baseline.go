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
// achieved (see PlayStat), not the move's demonstrated value: a recording
// shows a move where the demonstrator chose it, which says little about how
// it does when the CBR layer chooses it. Each play is kept under the move and
// the situation it was played in (see playKey), the decision situation the
// base AI is measured in too, whatever situation its case was recorded in:
// a move proves itself only where its own plays were made. A play whose
// command did not come out is not one of the move's plays (whatever the
// character did instead is not the move), and neither is a play the
// character's own AI took over from; a move whose plays it keeps taking over
// from is not tried any more (see Params.TakeoverContestMax).
//
// A move with no plays in a situation has proved nothing there, so the CBR
// layer now and then tries one whose demonstrations beat the base AI (see
// Params.TakeoverExplore), and only then does it start to take over with
// it. A few plays are noisy, so a move that has been played but has not
// proved better yet is tried again the same way, while it could still prove
// itself, until it has had a fair number of plays (see
// Params.TakeoverRetryPlays). Trying moves costs the base AI play, so it is
// rare and capped per round, and spent first where a move can prove itself
// in a few plays (see promising).
//
// Both sides are measured per decision, by the same rules: each decision,
// the base AI's or a move the CBR layer played, is credited with its share
// of the reward of the exchange it led into: all of it, less a factor of
// ComboGamma for each move the CBR layer played after it in the exchange, or
// none if it never led into one. The base AI's own later decisions do not
// reduce the share (see chain.split): it makes many more of them than the
// CBR layer, and counting them would favour the CBR layer's moves. Whether a
// decision led into an exchange at all is decided by the lead-in, which
// keeps the last few decisions before an exchange whichever layer made them,
// counting a move the CBR layer played as many decisions as the base AI
// would have made over it (see chain.go), so that a step in neutral made
// long before an exchange, with many decisions after it, counts as neutral
// on both sides.
//
// The base AI is compared where the move was on offer too. When the CBR
// layer plays a move in a situation, the base AI's result there is missing
// from its record, so where a move is often played the base AI's record
// leans toward the moments the move was not on offer, which may be worse
// ones (a hit at a point where no cancel starts, say). So each probe of a
// decision the CBR layer left to the base AI with moves on offer (the
// closest matches, see gate.offers) also goes into a record per situation
// and move (Store.Offered). A move is only played where it is on offer in
// the same sense. That record can lean the other way (a move may be among
// the closest mostly at moments nothing else matches well), so once it
// holds enough results a move must beat both it and the situation's
// record; until then, the situation's record at twice the margin (see
// Store.against).
//
// The comparison comes before the similarity band (see chooseWhy): of the
// moves that match at all, those allowed to take over are found first, and
// the band and the ranking choose among them. So a move that proved better
// from a distance takes over when the base AI walks, runs or jumps into that
// distance, though a case of the walk itself, whose recorded input matches
// the walk's, is closer. While the character moves with control, the CBR
// layer looks again every movingRetryFrames frames, and at once in each new
// situation, such as the next distance band (see driver.input); in a
// movement without control (a dash, the start of a jump, a landing) only a
// command verified from that very state may take over (see
// moveWithoutControl).
//
// The take-over margin is set per exchange, so it is scaled down by the
// share a decision in the situation usually gets (see baselineShare) before
// it is compared with a move's advantage. A margin of 0.03 is thus 3% of a
// life bar per exchange, whether the decision is the one that lands the
// knockdown or a step in neutral that seldom leads into an exchange. The
// evidence is counted in exchanges too: a move proves itself only once its
// plays have been credited with TakeoverMinPlays exchanges' worth of shares,
// so where decisions seldom lead into an exchange it needs many more plays,
// and one exchange, however large, proves nothing; and its advantage is
// shrunk by exchanges' worth as well (see Params.advantage).

// BaselineStat is the base AI's decayed record in one situation: how many
// results, their sum, and the sum of the shares of their exchanges' rewards
// they were credited with (see baselineShare).
type BaselineStat struct {
	Trials   float32
	ValueSum float32
	Shares   float32
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
	// The character's own stance: its state type in bits 15 to 17, and in
	// bits 18 and 19 whether it is being hit or guarding (see selfStun).
	baseSelfShift        = 15
	baseStunShift        = 18
	baseStance    uint64 = 7<<baseSelfShift | 3<<baseStunShift
)

// The layout of Store.Baseline, Store.Offered and Store.Played: 1 has the
// character's own stance in the situation; 2 keeps the CBR layer's plays by
// the situation they were played in, with the shares of their exchanges, and
// adds Offered. Stores kept in an older layout (0 is before the field) lose
// these records on load (see LoadStore), and keep their cases: their
// situations mixed what is now kept apart.
const baseFormat = 2

// baselineKey identifies the situation of a decision for the baseline:
// actionKey's description of the situation without the move (the enemy's
// state type, the distance band, the exchange context and control), plus the
// character's own stance and the AI level the base AI plays at. The stance
// says whether the character is standing, crouching, in the air or lying,
// and whether it is being hit or guarding: what the base AI does in the air
// is no measure of a ground move, and what it does while guarding no measure
// of what it can do while being comboed. A move is played only from the
// stance it was recorded in (see gateFilter), and its plays are kept by the
// situation they were made in (see playKey). The AI level matters because
// the base AI's strength depends on it.
func baselineKey(f *Feat, ctrl bool, aiLevel float32) uint64 {
	k := baseFine | uint64(f.Enemy.State&7)<<4 | distanceBand(f) |
		uint64(exchangeContext(f))<<8 | aiBand(aiLevel)<<13 |
		uint64(f.Self.State&7)<<baseSelfShift | selfStun(f)<<baseStunShift
	if ctrl {
		k |= 1 << 11
	}
	return k
}

// selfStun is 1 when the character is being hit, 2 when it is guarding, and
// 0 otherwise. Being hit and guarding look alike from the outside (the enemy
// attacks, the character has no control), but what can be done from them is
// very different: a character being comboed takes damage whatever it does,
// one guarding takes little and may soon act.
func selfStun(f *Feat) uint64 {
	switch {
	case f.Self.beingHit():
		return 1
	case f.Self.BlockStun > 0:
		return 2
	}
	return 0
}

// stanceOf is the stance a case is recorded in, in the bits baselineKey
// keeps it in.
func stanceOf(c *Case) uint64 {
	return uint64(c.StartStateType&7)<<baseSelfShift | selfStun(&c.Start)<<baseStunShift
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
// control, own state type and AI level band.
func coarseKey(k uint64) uint64 { return k&^(baseGrain|baseNear) | baseCoarse }

// decayed is the weight of n results in a row after recency decay, the most
// that n results can add up to.
func (p *Params) decayed(n float32) float32 {
	if p.Decay >= 1 || p.Decay <= 0 {
		return n
	}
	return float32((1 - math.Pow(float64(p.Decay), float64(n))) / (1 - float64(p.Decay)))
}

// addBaseline records one result r of the base AI in a situation, into the
// situation, its coarser situation and the store-wide record. share is the
// share of its exchange's reward the decision was credited with: 1, a factor
// of ComboGamma less for each move the CBR layer played after it in the
// exchange, 0 when it never led into one.
func (s *Store) addBaseline(p *Params, key uint64, r, share float32) {
	for _, k := range [...]uint64{key, coarseKey(key), baseAll} {
		b := s.Baseline[k]
		if b == nil {
			b = &BaselineStat{}
			s.Baseline[k] = b
		}
		b.Trials = b.Trials*p.Decay + 1
		b.ValueSum = b.ValueSum*p.Decay + r
		b.Shares = b.Shares*p.Decay + share
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

// baselineShare returns the share of its exchange's reward a decision in a
// situation is usually credited with (see addBaseline), from the base AI's
// record and shrunk like its mean, starting from a whole exchange: with
// little known about a situation, the margin stays at its full size.
func (s *Store) baselineShare(p *Params, key uint64) float32 {
	share := float32(1)
	for _, k := range [...]uint64{baseAll, coarseKey(key), key} {
		if b := s.Baseline[k]; b != nil && p.BaselineShrink+b.Trials > 0 {
			share = (p.BaselineShrink*share + b.Shares) / (p.BaselineShrink + b.Trials)
		}
	}
	return share
}

// bar is what a move must beat to take over from the base AI in situation
// key at take-over margin m (see takeoverMargin), by the situation's record
// alone: the base AI's mean b there; the margin, scaled to the share of an
// exchange's reward a decision there usually gets and doubled while the base
// AI's record there holds fewer than BaselineMinTrials results; that share;
// and whether the record holds enough results to compare with.
func (s *Store) bar(p *Params, key uint64, m float32) (b, margin, share float32, known bool) {
	b, n := s.baseline(p, key)
	share = s.baselineShare(p, key)
	margin = m * share
	known = n >= p.decayed(p.BaselineMinTrials)-1e-3
	if !known {
		margin *= 2
	}
	return b, margin, share, known
}

// basis is one record of the base AI that a move is compared with: its mean
// b, the margin a move must beat it by, and the share of an exchange's
// reward its decisions usually get, by which the margin is scaled and the
// evidence counted (see advantage).
type basis struct{ b, margin, share float32 }

// comparison is what a move must beat to take over from the base AI in a
// situation (see Store.against): one or two records of the base AI (the
// first n of bases), and whether the situation's record holds enough results
// to compare with.
type comparison struct {
	bases [2]basis
	n     int
	known bool
}

// against is what a move must beat to take over from the base AI at
// take-over margin m in the situation of k, a key of the move's records in
// it (see playKey). That is the base AI's record of the situation as a whole
// (see bar) and, once it holds BaselineMinTrials results, its record where
// the move was on offer there (Store.Offered), shrunk toward the situation's
// mean as if BaselineShrink results had matched it. A move must beat both,
// since each can lean away from the moments the move is played at: the
// situation's record toward the moments the move was not on offer (where it
// was played, the base AI's result is missing), the record where it was on
// offer toward the moments where nothing else matched as closely (where the
// move is in the band at all), while the move's plays come from wherever it
// was on offer once it had proved itself. While the record where the move
// was on offer is too thin, the situation's record is used alone, at twice
// the margin.
func (s *Store) against(p *Params, k uint64, m float32) comparison {
	b, margin, share, known := s.bar(p, k&sitMask, m)
	return s.onOffer(p, k, m, basis{b, margin, share}, known)
}

// onOffer is against, given the situation's record as a basis (see bar) and
// whether it holds enough results, so that a choice among many moves in one
// situation reads the situation's record once.
func (s *Store) onOffer(p *Params, k uint64, m float32, sit basis, known bool) comparison {
	o := s.Offered[k]
	if o == nil || o.Trials < p.decayed(p.BaselineMinTrials)-1e-3 {
		sit.margin = 2 * m * sit.share
		return comparison{bases: [2]basis{sit}, n: 1, known: known}
	}
	b := (p.BaselineShrink*sit.b + o.ValueSum) / (p.BaselineShrink + o.Trials)
	share := (p.BaselineShrink*sit.share + o.Shares) / (p.BaselineShrink + o.Trials)
	return comparison{bases: [2]basis{sit, {b, m * share, share}}, n: 2, known: known}
}

// beats reports whether plays rec beat every record of cmp by its margin,
// each advantage (see advantage) weighted by w when it is positive: the
// chance that the move's route completes, as for its value (see score).
func (p *Params) beats(cmp comparison, rec PlayStat, w float32) bool {
	for _, x := range cmp.bases[:cmp.n] {
		adv := p.advantage(rec, x.b, x.share)
		if adv > 0 {
			adv *= w
		}
		if adv <= x.margin {
			return false
		}
	}
	return true
}

// promises reports whether a move whose plays did as well as mean would
// soon prove itself against every record of cmp (see promising).
func (p *Params) promises(cmp comparison, mean float32) bool {
	for _, x := range cmp.bases[:cmp.n] {
		if !p.promising(mean, x.b, x.share, x.margin) {
			return false
		}
	}
	return true
}

// strictest is the record of cmp with the highest mean: what an unproven
// move's own results must beat to be worth trying (see explorable).
func (c comparison) strictest() basis {
	x := c.bases[0]
	if c.n > 1 && c.bases[1].b > x.b {
		x = c.bases[1]
	}
	return x
}

// advantage is how much better per decision than the base AI's mean b the
// plays in rec did, shrunk toward zero as if TakeoverShrink exchanges' worth
// of further plays had matched the base AI exactly. Where a decision
// usually gets the share share of an exchange's reward, an exchange's worth
// is 1/share plays: in neutral, where most decisions never lead into an
// exchange, a move's plays prove something only once enough of them have,
// which takes many more plays than in a combo.
func (p *Params) advantage(rec PlayStat, b, share float32) float32 {
	return (rec.Sum - rec.Trials*b) / (p.TakeoverShrink/max(share, 1e-6) + rec.Trials)
}

// proves reports whether plays rec are enough evidence to prove a move at
// all: TakeoverMinPlays plays, credited with at least TakeoverMinPlays
// exchanges' worth of shares between them, so that a single exchange,
// however large, among a few plays that led into none proves nothing.
// Counts are decayed, so n in a row add up to decayed(n), less a hair for
// rounding.
func (p *Params) proves(rec PlayStat) bool {
	n := p.decayed(p.TakeoverMinPlays) - 1e-3
	return rec.Trials >= n && rec.Shares >= n
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
	key       uint64 // the situation (see baselineKey)
	explore   bool   // a move not played often enough yet may be tried
	promising bool   // such a move may be tried if it is promising (see promising)
	// retry: a move played a few times without proving better may be tried
	// again, as explore and promising allow (the round's TakeoverRetryMax is
	// not used up)
	retry bool
	yield bool // leave even a move that proved better to the base AI
	// test: a move whose command must be verified (see Case.mustVerify) and
	// is not yet verified here may be played as a test (the round's
	// Params.VerifyPerRound is not used up).
	test bool
	// self is the character at the choice, whose spot the verdicts are read
	// in (see Verdict); chooseWhy sets it.
	self *CharSnap
	// Set by the choice: held when moves matched but none may take over, so
	// the base AI keeps control; explored when the move chosen is an
	// unproven one tried by exploration, and retried as well when it had
	// been played before without proving better; tested when it is a test
	// play of a move not verified here yet.
	held, explored, retried, tested bool
	// offers is set by the choice too: the moves on offer, as keys of their
	// records in the situation (see playKey), the closest matches first (by
	// the similarity take-overs are ranked by, see takeoverDist), one for
	// each move, at most offerMax of them. Only these may take over or be
	// tried, and when the base AI keeps control, its result there is
	// credited to them as well (see Store.Offered), so that a move is played
	// and compared at the same kind of moments.
	offers [offerMax]uint64
}

// offerMax is how many moves may be on offer at a decision: the closest of
// those that match within the confidence threshold, one for each move. It
// bounds how many records a decision left to the base AI is credited to
// (see Store.Offered); with fewer moves matching than this, every one of
// them is on offer.
const offerMax = 8

// offer notes the move whose records in the situation key k names, matched
// at distance dist, among the moves on offer, keeping the offerMax closest
// in order, one entry for each move; dists holds their distances.
func (g *gate) offer(k uint64, dist float32, dists *[offerMax]float32) {
	i := 0
	for ; i < offerMax && g.offers[i] != 0; i++ {
		if g.offers[i] == k {
			if dist >= dists[i] {
				return
			}
			break
		}
	}
	if i == offerMax {
		i = offerMax - 1
		if dist >= dists[i] {
			return
		}
	}
	// Slot i is free to take the move: the move's own slot, an empty one, or
	// the farthest one's. The moves before it that matched less closely move
	// down a place.
	for ; i > 0 && dists[i-1] > dist; i-- {
		g.offers[i], dists[i] = g.offers[i-1], dists[i-1]
	}
	g.offers[i], dists[i] = k, dist
}

// onOffer reports whether the move whose records in the situation key k
// names is on offer (see offers).
func (g *gate) onOffer(k uint64) bool {
	for _, o := range g.offers {
		if o == k {
			return true
		}
	}
	return false
}

// Rejection reasons of the gate, counted by chooseWhy.
const (
	// The move has not proved better than the base AI here.
	RejNotBetter = "not_better"
	// The move proved better, but this situation is left to the base AI to
	// keep its record current (see Params.BaseExplore).
	RejBaseExplore = "base_explore"
	// The move matched, but offerMax closer moves were on offer (see
	// gate.offers).
	RejNotOnOffer = "not_on_offer"
	// The move's command must be verified (see Case.mustVerify, and every
	// command in a movement without control, see moveWithoutControl) and is
	// not verified here yet, and the round's test plays are used up.
	RejUnverified = "unverified"
)

// gateFilter keeps the candidates that may take over from the base AI: the
// moves the CBR layer has played often enough in this situation whose
// results there beat the base AI's by the margin (see Params.proves and
// Store.against). When none does and exploring is allowed, it keeps instead
// the moves that may be tried to prove themselves (see explorable): those
// not played often enough here yet to have proved anything, or else, if
// there are none, those played a few times whose results neither proved
// better nor fell short of the base AI's by more than the margin, until they
// have TakeoverRetryPlays plays. A move may be promising (see promising),
// played or not, and is then tried with the larger chance. A move whose
// plays here the character's own AI has taken over from TakeoverContestMax
// times is not tried either way.
//
// Only the moves on offer are considered (see gate.offers): the closest
// moves of those that match, so that a move is played at the same kind of
// moments as those where the base AI's results are compared with it (see
// Store.Offered). The candidates are all those within the confidence
// threshold, not only those within the similarity band of the best match:
// chooseWhy applies the band afterwards, among the moves let through. A
// move is considered only in the stance it was recorded in: a step back
// recorded standing is not played from a crouch, nor a move recorded while
// guarding where the character is being hit.
//
// A move whose command nobody was seen to input (see Case.mustVerify) never
// takes over as proved better where its command has not been verified (see
// Verdict): there it is played as a test, while the round's test plays last
// (see Params.VerifyPerRound), ahead of any exploration and whatever its
// results so far, or else tried by exploration as above; either play
// verifies its command there if the command comes out. The same holds for
// every case in a movement without control (see moveWithoutControl), where
// a command is verified from the very state the character is in: a person's
// recording shows the command working where it was recorded, not that the
// CBR layer's input of it works from this point of a dash. Where its command
// was found not to work, chooseWhy left it out already.
func (d *driver) gateFilter(picks []pick, g *gate, ambExp float64, rej func(string)) []pick {
	p, st := d.p, d.st
	minPlays := p.decayed(p.TakeoverMinPlays) - 1e-3
	retryPlays := p.decayed(p.TakeoverRetryPlays) - 1e-3
	maxContests := p.decayed(p.TakeoverContestMax) - 1e-3
	noCtrl := moveWithoutControl(g.self)
	sb, smargin, sshare, sknown := st.bar(p, g.key, d.margin)
	sit := basis{sb, smargin, sshare}
	// The moves on offer, from the candidates in the stance they were
	// recorded in; keys holds each candidate's key, 0 for the others.
	var dists [offerMax]float32
	g.offers = [offerMax]uint64{}
	keys := d.gateKeys[:0]
	for _, pk := range picks {
		k := uint64(0)
		if stanceOf(pk.c) == g.key&baseStance {
			k = playKey(pk.c, g.key)
			g.offer(k, pk.dist, &dists)
		}
		keys = append(keys, k)
	}
	d.gateKeys = keys
	var proven, untried, again, tests []pick
	for i, pk := range picks {
		c, k := pk.c, keys[i]
		if k == 0 {
			rej(RejExecCond)
			continue
		}
		if !g.onOffer(k) {
			rej(RejNotOnOffer)
			continue
		}
		cmp := st.onOffer(p, k, d.margin, sit, sknown)
		top := cmp.strictest()
		a := st.playRecord(k)
		tryable := a.Contests < maxContests
		unverified := (c.mustVerify() || noCtrl) && st.check(c, g.self) != verdictVerified
		if unverified && g.test && tryable {
			tests = append(tests, pk)
			continue
		}
		if a.Trials >= minPlays {
			if !unverified && p.proves(a) && p.beats(cmp, a, float32(math.Pow(float64(d.completion(c)), ambExp))) {
				proven = append(proven, pk)
				continue
			}
			// Not proved better yet. A few plays are noisy, so it may be
			// tried again while it could still prove itself: with the larger
			// chance when its plays so far are promising (only the shrinking
			// holds it back), with the ordinary chance when they are within the
			// margin of the base AI's and its case's record (demonstrated and
			// played) is promising. A move that could not prove itself in
			// TakeoverRetryPlays plays even if it kept doing as well, a step in
			// neutral far from the enemy say, is not tried again: the round's
			// one retry is kept for one that could.
			mean := a.Sum / a.Trials
			soon := cmp.known && p.promises(cmp, mean)
			could := soon || c.Outcome.Trials > 0 && p.promises(cmp, c.Outcome.ValueSum/c.Outcome.Trials)
			if g.retry && tryable && a.Trials < retryPlays && explorable(c, top.b) &&
				(g.promising && soon || g.explore && mean-top.b > -top.margin && could) {
				again = append(again, pk)
				continue
			}
		} else if tryable && (g.explore && explorable(c, top.b) ||
			g.promising && cmp.known && c.Outcome.Trials > 0 && p.promises(cmp, c.Outcome.ValueSum/c.Outcome.Trials)) {
			untried = append(untried, pk)
			continue
		}
		if unverified {
			rej(RejUnverified)
			continue
		}
		rej(RejNotBetter)
	}
	switch {
	case len(proven) > 0 && g.yield:
		for range proven {
			rej(RejBaseExplore)
		}
		for range tests {
			rej(RejUnverified)
		}
		g.held = true
		return nil
	case len(proven) > 0:
		for range tests {
			rej(RejUnverified)
		}
		return proven
	case len(tests) > 0:
		for range untried {
			rej(RejNotBetter)
		}
		for range again {
			rej(RejNotBetter)
		}
		g.tested = true
		return tests
	case len(untried) > 0:
		for range again {
			rej(RejNotBetter)
		}
		g.explored = true
		return untried
	case len(again) > 0:
		g.explored, g.retried = true, true
		return again
	}
	g.held = true
	return nil
}

// explorable reports whether an unproven move may be tried by exploration:
// the case's own result, as demonstrated (and as played, if it has been),
// beats the base AI's mean b. A case with no result at all has nothing to
// compare and may always be tried: a hypothesis, which has none until it is
// played, or a route recorded in trial mode, whose recording is not graded.
func explorable(c *Case, b float32) bool {
	if c.Outcome.Trials > 0 {
		return c.Outcome.ValueSum/c.Outcome.Trials > b
	}
	return true
}

// promising reports whether a move not proved better yet looks likely to
// prove itself soon, so it may be tried more often (see
// Params.TakeoverExplorePromising): if its plays did as well as mean (its
// demonstrated result, or the mean of its own plays once it has a few) and
// were credited with the usual share share of an exchange's reward,
// TakeoverRetryPlays of them would prove it better than the base AI's mean b
// by the margin (see proves and advantage). The larger chance is given only
// where the base AI's record holds enough results to compare with. In
// neutral, where decisions seldom lead into an exchange, few moves are:
// proving one there takes many more plays than a round's explorations,
// which are better spent where a move can prove itself, such as a follow-up
// after a hit.
func (p *Params) promising(mean, b, share, margin float32) bool {
	n := p.decayed(p.TakeoverRetryPlays)
	rec := PlayStat{Trials: n, Sum: n * mean, Shares: n * share}
	return p.proves(rec) && p.advantage(rec, b, share) > margin
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
// AI are drawn once, so later choices in the same situation do not draw
// again. One draw decides both kinds of exploration: a promising move is
// tried when it falls under TakeoverExplorePromising, any other when it
// falls under TakeoverExplore.
func (d *driver) enter(sit decisionSit) {
	if d.sitSeen && sit == d.sit {
		return
	}
	d.sit, d.sitSeen, d.sitProbed, d.spanned = sit, true, false, false
	p := d.p
	d.sitExplore, d.sitDraw = false, 1
	if p.TakeoverExplore > 0 && d.explores < p.TakeoverExploreMax {
		d.sitDraw = d.rng.Float32()
		d.sitExplore = d.sitDraw < p.TakeoverExplore || d.sitDraw < p.TakeoverExplorePromising
	}
	d.sitYield = p.BaseExplore > 0 && d.rng.Float32() < p.BaseExplore
}

// decide makes a choice at a decision point. With the base AI on, only a
// move that proved better than the base AI here takes over (see
// gateFilter), apart from the rare tries of moves not proved yet and the
// test plays of moves whose command is not verified here yet; when moves
// matched but none may, the base AI keeps control, and a probe follows how
// it does from here, for the situation and for the closest moves that were
// on offer.
func (d *driver) decide(cur *Feat, self *CharSnap, tick int64) *Case {
	d.held = false
	if !d.p.BaseAI {
		c, _ := d.choose(cur, self, tick)
		return c
	}
	key := baselineKey(cur, self.Ctrl, self.AILevel)
	d.enter(decisionSit{key, self.StateNo, self.Ctrl})
	g := gate{key: key, yield: d.sitYield, test: d.tests < d.p.VerifyPerRound}
	if d.sitExplore {
		g.explore = d.sitDraw < d.p.TakeoverExplore
		g.promising = d.sitDraw < d.p.TakeoverExplorePromising
		g.retry = d.retries < d.p.TakeoverRetryMax
	}
	c, _ := d.chooseWhy(cur, self, tick, nil, &g)
	if c != nil && g.tested {
		d.tests++
	}
	if c != nil && g.explored {
		d.explores++
		d.sitExplore = false
		if g.retried {
			d.retries++
		}
	}
	switch {
	case g.held:
		d.held = true
		d.probe(key, tick, g.offers)
	case c != nil && d.sitProbed:
		// The CBR layer takes over in a situation it first left to the base
		// AI, before the situation changed (a cancel becomes possible a frame
		// later, say): the decision is the CBR layer's, not the base AI's.
		d.ct.withdraw(key, d.probeTick)
		d.sitProbed = false
	}
	return c
}

// probe adds a probe for the current decision situation, once, crediting the
// moves on offer there as well (see Store.Offered).
func (d *driver) probe(key uint64, tick int64, offers [offerMax]uint64) {
	if d.sitProbed {
		return
	}
	d.sitProbed, d.probeTick = true, tick
	d.unspan(tick)
	d.ct.addCase(chainCase{probe: true, key: key, startTick: tick, offers: offers})
}

// baseAction adds a probe for an action the base AI started on its own from
// the decision frame before (self, cur), unless the base AI's decision there
// already has one.
func (d *driver) baseAction(cur *Feat, self *CharSnap, tick int64) {
	key := baselineKey(cur, self.Ctrl, self.AILevel)
	if sit := (decisionSit{key, self.StateNo, self.Ctrl}); d.sitSeen && sit == d.sit {
		d.probe(key, tick, [offerMax]uint64{})
		return
	}
	d.ct.addCase(chainCase{probe: true, key: key, startTick: tick})
}

// follow notes, on a frame of a play, the decision situation the character
// is in, so that the play counts for as many decisions as the base AI would
// have made over it. Playing on its own, the base AI makes a decision (and
// gets a probe) in each new decision situation it has control in, such as
// each distance band a walk crosses; a play of the CBR layer's is one
// decision however long it lasts. So each new decision situation with
// control that a play goes through counts as one more decision of the play
// in the lead-in (see chainTracker.extend), and the play's start is as far
// from the next exchange as the base AI's own first decision there would be.
// The situation becomes the driver's current one, decided by the play:
// nothing is drawn for it, and a decision made in it once the play is over,
// or a new play begun in it, takes over the play's count of it (see unspan).
func (d *driver) follow(cur *Feat, self *CharSnap, tick int64) {
	if d.cur == nil || !self.Ctrl {
		return
	}
	sit := decisionSit{baselineKey(cur, self.Ctrl, self.AILevel), self.StateNo, true}
	if d.sitSeen && sit == d.sit {
		return
	}
	d.sit, d.sitSeen, d.sitProbed = sit, true, false
	d.sitExplore, d.sitDraw, d.sitYield = false, 1, false
	d.spanned, d.spanID, d.spanStart = true, d.cur.ID, d.curStart
	d.ct.extend(d.cur.ID, d.curStart, 1, tick)
}

// unspan hands the current decision situation, which the last play counted
// as one of its decisions (see follow), to the decision now made in it: the
// play counts one decision fewer, and the new one counts for itself.
func (d *driver) unspan(tick int64) {
	if d.spanned {
		d.spanned = false
		d.ct.extend(d.spanID, d.spanStart, -1, tick)
	}
}

// SituationString describes a baseline key, for inspection.
func SituationString(k uint64) string {
	if k&baseGrain == baseAll {
		return "all situations"
	}
	ai := [...]string{"AI level 1-3", "AI level 4-6", "AI level 7-8", "AI level ?"}[k>>13&3]
	s := contextString(k>>8&3) + ", " + ctrlString(k&(1<<11) != 0) + ", " + ai
	if k&baseGrain != baseCoarse {
		s = enemyString(k) + ", " + s
	}
	return selfString(k) + ", " + s
}

// selfString describes the character's own stance in a baseline key.
func selfString(k uint64) string {
	return stanceString(k>>baseSelfShift&7, k>>baseStunShift&3)
}

// stanceString describes a stance: a state type, and whether the character
// is being hit or guarding (see selfStun).
func stanceString(state, stun uint64) string {
	s := "self in no known state"
	switch StateType(state) {
	case StStand:
		s = "self standing"
	case StCrouch:
		s = "self crouching"
	case StAir:
		s = "self airborne"
	case StLying:
		s = "self lying"
	}
	switch stun {
	case 1:
		s += " and being hit"
	case 2:
		s += " and guarding"
	}
	return s
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
