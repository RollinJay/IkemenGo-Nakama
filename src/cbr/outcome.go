package cbr

// applyAction pools one result into the case's action statistics, with
// the same recency decay as the case's own record.
func (p *Params) applyAction(a *ActionStat, r float32, opp string, trial bool) {
	if trial {
		a.Trials = a.Trials*p.Decay + 1
		a.ValueSum = a.ValueSum*p.Decay + r
	} else {
		a.ValueSum += r
	}
	if opp != "" {
		if a.ByOpp == nil {
			a.ByOpp = map[string]OppStat{}
		}
		s := a.ByOpp[opp]
		if trial {
			s.Trials = s.Trials*p.Decay + 1
			s.ValueSum = s.ValueSum*p.Decay + r
		} else {
			s.ValueSum += r
		}
		a.ByOpp[opp] = s
	}
}

// actionValue is the selection value of a case: its action's pooled mean,
// shrunk toward the case's source prior, then toward the per-opponent
// record when one exists. Every instance of an action shares this value,
// so selection among them falls to similarity.
func (p *Params) actionValue(st *Store, c *Case, opp string) float32 {
	pr := p.Priors[c.Source]
	a := st.Actions[actionKey(c)]
	if a == nil {
		return pr.Value
	}
	v := pr.Value
	if d := pr.Weight + a.Trials; d > 0 {
		v = (pr.Weight*pr.Value + a.ValueSum) / d
	}
	if opp != "" && a.ByOpp != nil {
		if s, ok := a.ByOpp[opp]; ok && s.Trials > 0 {
			const k = 5
			v = (v*k + s.ValueSum) / (k + s.Trials)
		}
	}
	return v
}

// applyOutcome records one measured result for a played case. Existing
// sums decay first, so recent results count more than old ones and the
// model follows an opponent who adapts.
func (p *Params) applyOutcome(c *Case, r float32, opp string, tick int64) {
	o := &c.Outcome
	o.Trials = o.Trials*p.Decay + 1
	o.ValueSum = o.ValueSum*p.Decay + r
	o.ValueSq = o.ValueSq*p.Decay + r*r
	o.LastTick = tick
	if opp != "" {
		if o.ByOpp == nil {
			o.ByOpp = map[string]OppStat{}
		}
		s := o.ByOpp[opp]
		s.Trials = s.Trials*p.Decay + 1
		s.ValueSum = s.ValueSum*p.Decay + r
		o.ByOpp[opp] = s
	}
}

// applyCredit adds credit to a case's value without counting a new trial.
// Used for the round's terminal reward, which belongs to trials already
// counted when their chains resolved.
func applyCredit(c *Case, r float32, opp string) {
	c.Outcome.ValueSum += r
	if opp != "" && c.Outcome.ByOpp != nil {
		if s, ok := c.Outcome.ByOpp[opp]; ok {
			s.ValueSum += r
			c.Outcome.ByOpp[opp] = s
		}
	}
}

// recordPerturbedDrop keeps a failure caused by injected execution noise
// out of the case's value and in per-level statistics instead. Otherwise
// low-difficulty play would teach the store that good routes are bad.
func recordPerturbedDrop(c *Case, level int, dropped bool) {
	if c.Outcome.ByLevel == nil {
		c.Outcome.ByLevel = map[int8]LevelStat{}
	}
	s := c.Outcome.ByLevel[int8(level)]
	s.Attempts++
	if dropped {
		s.Drops++
	}
	c.Outcome.ByLevel[int8(level)] = s
}

// matchupValue blends the pooled value with the per-opponent record,
// shrinking toward the pool while the opponent record is thin.
func (p *Params) matchupValue(c *Case, opp string) float32 {
	pooled := c.Value(p)
	if opp == "" || c.Outcome.ByOpp == nil {
		return pooled
	}
	s, ok := c.Outcome.ByOpp[opp]
	if !ok || s.Trials <= 0 {
		return pooled
	}
	const k = 5
	return (pooled*k + s.ValueSum) / (k + s.Trials)
}

// terminalShares distributes a round's result over its chains, most
// recent first, decaying by RoundGamma per chain. Only chains that began in
// the late-round pressure zone receive any: the terminal reward exists to
// teach clock play, and crediting every decision in a round with the
// round's result is high-variance noise that measurably degraded play in
// testing. Late in a round the result is causally tied to individual
// decisions such as stalling, blocking and spacing out. The clock pays out
// only through this terminal reward, never per frame: a stall earns nothing
// by itself and is credited only when it actually wins.
func (p *Params) terminalShares(chains []*resolvedChain, won, lost bool) []float32 {
	out := make([]float32, len(chains))
	base := float32(0)
	switch {
	case won:
		base = p.TerminalWin
	case lost:
		base = p.TerminalLoss
	}
	if base == 0 || !p.ClockEnabled {
		return out
	}
	g := float32(1)
	for i := len(chains) - 1; i >= 0; i-- {
		tf := chains[i].ch.timeFrac0
		if tf < 0 {
			continue // infinite timer: no clock to play
		}
		pressure := 1 - tf
		if pressure > p.ClockPressureFloor {
			ramp := (pressure - p.ClockPressureFloor) / (1 - p.ClockPressureFloor)
			out[i] = base * p.TerminalWeight * g * (1 + p.ClockWeightMax*ramp)
			g *= p.RoundGamma
		}
	}
	return out
}
