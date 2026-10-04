package cbr

// enderKind classifies the last attack of a chain. A super is identified by
// meter spent during the move, falling back to the common state-number
// convention (3000 and up); a special by the 1000-2999 convention.
func enderKind(stateNo int32, spentPower bool) CompletionKind {
	switch {
	case spentPower:
		return CompSuper
	case stateNo >= 3000 && stateNo < 5000:
		return CompSuper
	case stateNo >= 1000 && stateNo < 3000:
		return CompSpecial
	case stateNo >= 200:
		return CompNormal
	}
	return CompUnknown
}

// resultState classifies the state a chain left the focus in.
func resultState(c *chain, frameAdv int32, roundEnded, enemyKO, selfKO, pressure bool) CompletionKind {
	switch {
	case roundEnded && enemyKO:
		return CompRoundEnd
	case selfKO:
		return CompPunished
	case c.punished:
		return CompPunished
	case c.timedOut:
		return CompTimeout
	case c.sawDown:
		return CompKnockdown
	case frameAdv >= 2 || pressure:
		return CompAdvantage
	case frameAdv <= -2:
		if c.dealt > 0 {
			return CompDropped
		}
		return CompNeutral
	}
	return CompNeutral
}

// quality returns the multiplier on banked damage for a result.
func (p *Params) quality(k CompletionKind) float32 {
	if q, ok := p.Quality[k]; ok {
		return q
	}
	return p.Quality[CompNeutral]
}

// reward converts a resolved chain into a scalar. Completion multiplies the
// damage dealt rather than adding to it, so damage dealt into a punishable
// ending is discounted instead of rewarded. A small additive situation term
// ranks chains that dealt no damage, such as defence and spacing.
func (p *Params) reward(comp *Completion) float32 {
	r := comp.DamageDealt * p.quality(comp.Result)
	r -= comp.DamageTaken * p.TakenWeight
	r += comp.MeterDelta * p.MeterWeight
	r += p.Situation[comp.Result]
	return r
}
