package cbr

import "math"

// Selection: filter by hard constraints, score what remains, and decline
// when nothing is close enough so the engine's own AI keeps control.

func grounded(st StateType) bool { return st == StStand || st == StCrouch }

// execCompatible checks a case's execution conditions against the current
// state of the character that would play it.
func execCompatible(c *Case, self *CharSnap, cur *Feat) bool {
	switch {
	case c.StartStateType == StAir:
		if self.StateType != StAir {
			return false
		}
	case c.StartStateType == StLying:
		if self.StateType != StLying {
			return false
		}
	case grounded(c.StartStateType):
		if !grounded(self.StateType) {
			return false
		}
		// Standing and crouching share candidates, but the same button gives
		// a different move in each, and a jump needs a standing start: a
		// command case plays only from the stance it was recorded in.
		if c.ExecFrame >= 0 && self.StateType != c.StartStateType {
			return false
		}
	default:
		return false
	}
	if c.StartCtrl {
		return self.Ctrl
	}
	// Began mid-action: a cancel, a link, or a reversal buffered in stun.
	// Its input timing is relative to that action, so it must start from
	// the same state at the same point in it.
	if self.Ctrl || self.MoveType != c.StartMoveType || self.StateNo != c.StartStateNo {
		return false
	}
	// Never earlier than recorded, at most midActionSlack frames later.
	// State time stands still during hitpause, so the remaining hitpause
	// places the frame within it.
	dt := self.StateTime - c.Start.Self.StateTime
	if dt < 0 || dt > midActionSlack {
		return false
	}
	if (c.StartHitPause > 0 || self.HitPause > 0) &&
		(self.HitPause > c.StartHitPause || self.HitPause < c.StartHitPause-midActionSlack) {
		return false
	}
	// Hit confirmation: a cancel recorded after a hit is not attempted after
	// a whiff or a block, which most games would not allow anyway.
	return self.MoveType != MtAttack || cur.Contact == c.Start.Contact
}

// midActionSlack is how many frames after its recorded point in the action
// a mid-action case may still start.
const midActionSlack = 1

func stateBuckets(st StateType) []StateType {
	switch st {
	case StStand, StCrouch:
		return []StateType{StStand, StCrouch}
	case StAir:
		return []StateType{StAir}
	case StLying:
		return []StateType{StLying}
	}
	return nil
}

type pick struct {
	c     *Case
	dist  float32
	score float32
}

// choose returns the best case for the current state, or nil when nothing
// passes the filters within the confidence threshold.
func (d *driver) choose(cur *Feat, self *CharSnap, tick int64) (*Case, float32) {
	return d.chooseWhy(cur, self, tick, nil)
}

// Rejection reasons counted by chooseWhy, for the debug overlay.
const (
	RejRoundState = "round_state"
	RejExecCond   = "exec_condition"
	RejGate       = "technique_gate"
	RejKnowledge  = "knowledge"
	RejTrial      = "trial_opener"
	RejCancel     = "cancel_unseen"
	RejAmbition   = "ambition_length"
	RejReach      = "out_of_reach"
	RejDistance   = "too_far"
	// A case marked removed since the last index, or whose command keeps
	// failing to come out when played.
	RejUnreproducible = "unreproducible"
)

// chooseWhy is choose, counting why candidates were rejected into why when
// it is non-nil.
func (d *driver) chooseWhy(cur *Feat, self *CharSnap, tick int64, why map[string]int) (*Case, float32) {
	rej := func(r string) {
		if why != nil {
			why[r]++
		}
	}
	p, st, lv := d.p, d.st, d.levels
	evMin := st.evidenceThreshold(p, lv.Knowledge)
	lnN := math.Log(float64(st.total) + 1)
	ambExp := ambitionExponent(lv.Ambition)
	minReach := minReachSamples(lv.Knowledge)
	minEdge := minEdgeCount(lv.Knowledge)
	continuation := cur.ComboMoves > 0 && cur.DidHit

	var picks []pick
	bestScore := float32(math.MaxFloat32)
	bestDist := float32(math.MaxFloat32)
	for _, bucket := range stateBuckets(self.StateType) {
		for _, c := range st.candidates(bucket) {
			if c.Removed || c.Outcome.unreproducible() {
				rej(RejUnreproducible)
				continue
			}
			if c.Start.RoundState != cur.RoundState {
				rej(RejRoundState)
				continue
			}
			if !execCompatible(c, self, cur) {
				rej(RejExecCond)
				continue
			}
			if !p.gateOpen(c.Technique.Flags, lv) {
				rej(RejGate)
				continue
			}
			if evMin >= 0 && c.Evidence(p) < evMin {
				rej(RejKnowledge)
				continue
			}
			if c.IsTrial() && !(continuation || c.Kind == KindMovement || cur.Enemy.State == StAir) {
				rej(RejTrial)
				continue
			}
			// Attacking now: a case recorded with control needs the cancel from
			// the current attack to have been seen; a recorded cancel needs
			// its own cancel seen often enough for the knowledge level. A
			// case buffered during recovery plays from the very state it was
			// recorded in and needs no cancel.
			if c.ExecAttack && self.MoveType == MtAttack {
				from, need := self.StateNo, c.StartCtrl
				if c.CancelFrom != 0 {
					from, need = c.CancelFrom, true
				}
				if need && !st.allowsCancel(from, c.MoveRef, minEdge) {
					rej(RejCancel)
					continue
				}
			}
			if lv.Ambition < 8 && countPresses(c) > maxLinks(lv.Ambition) {
				rej(RejAmbition)
				continue
			}
			if c.ExecAttack && cur.Enemy.Present {
				// Project the distance at which the attack will come out: the
				// case closes (or opens) distance between its start and its
				// exec frame, e.g. walking in before pressing.
				dx := cur.Enemy.RelX
				if c.Reach.Valid && c.Start.Enemy.Present {
					dx -= c.Start.Enemy.RelX - c.Reach.DX
				}
				if prob, known := st.connect(c.MoveRef, cur.Enemy.State, dx, minReach); known && prob < 0.15 {
					rej(RejReach)
					continue
				}
			}
			dist := p.distance(cur, &c.Start)
			if dist > p.ConfidenceThreshold {
				rej(RejDistance)
				continue
			}
			if dist < bestDist {
				bestDist = dist
			}
			picks = append(picks, pick{c: c, dist: dist})
		}
	}
	if len(picks) == 0 {
		return nil, 0
	}
	// Stage two: among cases within the similarity band of the best match,
	// rank by value, exploration and reuse.
	band := bestDist + p.SimilarityBand
	human := false
	for _, pk := range picks {
		if pk.dist <= band && pk.c.Source.IsHuman() {
			human = true
			break
		}
	}
	kept := picks[:0]
	for _, pk := range picks {
		if pk.dist > band {
			continue
		}
		pk.score = d.score(pk.c, pk.dist, lnN, ambExp, tick)
		if human && !pk.c.Source.IsHuman() {
			pk.score += p.HumanPrecedence
		}
		if pk.score < bestScore {
			bestScore = pk.score
		}
		kept = append(kept, pk)
	}
	picks = kept
	d.lastBest = bestScore
	d.lastBestDist = bestDist
	tie := bestScore + p.TopSelectionThreshold
	var top []pick
	for _, pk := range picks {
		if pk.score <= tie {
			top = append(top, pk)
		}
	}
	if len(top) == 0 {
		return nil, 0 // scores were not comparable (NaN in the features)
	}
	if len(top) == 1 || d.rng == nil {
		return top[0].c, top[0].dist
	}
	pk := top[d.rng.Intn(len(top))]
	return pk.c, pk.dist
}

// score ranks a candidate: distance, plus the recent-use cost, minus
// learned value weighted by the route's completion probability at this
// execution level and ambition, minus an exploration bonus that shrinks as
// the case accumulates trials.
func (d *driver) score(c *Case, dist float32, lnN, ambExp float64, tick int64) float32 {
	return d.scoreOpt(c, dist, lnN, ambExp, tick, true)
}

func (d *driver) scoreOpt(c *Case, dist float32, lnN, ambExp float64, tick int64, reuse bool) float32 {
	p := d.p
	v := p.actionValue(d.st, c, d.opp)
	if v > 0 {
		v *= float32(math.Pow(float64(d.completion(c)), ambExp))
	}
	ucb := float32(math.Sqrt(lnN / float64(c.Outcome.Trials+1)))
	s := p.BandDistanceWeight*dist - p.ValueWeight*v - p.ExplorationWeight*ucb
	if reuse {
		s += d.st.reuseCost(p, c.ID, tick)
	}
	return s
}

// currentScore re-scores the case being played against the current state,
// for the better-case check. The reuse cost is left out: the case is in
// use because it was chosen, not because it is being repeated.
func (d *driver) currentScore(cur *Feat) float32 {
	c := d.cur
	lnN := math.Log(float64(d.st.total) + 1)
	dist := d.p.distance(cur, &c.Start)
	s := d.scoreOpt(c, dist, lnN, ambitionExponent(d.levels.Ambition), 0, false)
	// A case that no longer matches within the band of the new best is
	// outranked regardless of value.
	if dist > d.lastBestDist+d.p.SimilarityBand {
		return float32(math.MaxFloat32)
	}
	return s
}

func countPresses(c *Case) int {
	n := 0
	var prev InputBits
	for _, b := range c.Inputs {
		if b.Presses(prev) != 0 {
			n++
		}
		prev = b
	}
	return n
}

// completion returns the cached completion probability of a case's route
// at the driver's execution level.
func (d *driver) completion(c *Case) float32 {
	if d.p.noiseScale(d.levels.Execution) == 0 {
		return 1
	}
	key := c.Route.Key
	if key == 0 {
		return d.p.completionProb(c.Route.Links, d.levels.Execution)
	}
	if v, ok := d.cpCache[key]; ok {
		return v
	}
	links := c.Route.Links
	if rs := d.st.Routes[key]; rs != nil {
		links = rs.LinkStats()
	}
	v := d.p.completionProb(links, d.levels.Execution)
	if d.cpCache == nil {
		d.cpCache = map[uint64]float32{}
	}
	d.cpCache[key] = v
	return v
}
