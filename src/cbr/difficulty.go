package cbr

import "math"

// Levels are the difficulty dials, each 1 to 8. Ambition defaults to the
// execution level when zero.
type Levels struct {
	Execution int
	Ambition  int
	Knowledge int
}

func (l Levels) clamp() Levels {
	c := func(v, def int) int {
		if v == 0 {
			v = def
		}
		if v < 1 {
			return 1
		}
		if v > 8 {
			return 8
		}
		return v
	}
	l.Execution = c(l.Execution, 8)
	l.Ambition = c(l.Ambition, l.Execution)
	l.Knowledge = c(l.Knowledge, 8)
	return l
}

// Tier9 reports the top tier: execution and knowledge both at 8.
func (l Levels) Tier9() bool { return l.Execution >= 8 && l.Knowledge >= 8 }

// noiseScale multiplies measured human timing spread. Zero at execution 8
// (canonical timing), 1 roughly matches the recorded human, NoiseScaleMax at
// execution 1.
func (p *Params) noiseScale(exec int) float32 {
	if exec >= 8 {
		return 0
	}
	if exec < 1 {
		exec = 1
	}
	x := float64(8-exec) / 7
	return p.NoiseScaleMax * float32(math.Pow(x, float64(p.NoiseCurve)))
}

// knowledgeFrac is the fraction of data visible at a knowledge level.
func (p *Params) knowledgeFrac(k int) float32 {
	if k >= 8 {
		return 1
	}
	if k < 1 {
		k = 1
	}
	return p.KnowledgeFloor + (1-p.KnowledgeFloor)*float32(k-1)/7
}

// minReachSamples is how many observations a reach bin needs before it is
// trusted; fewer at high knowledge. Untrusted bins fall back inward.
func minReachSamples(k int) int32 {
	if k >= 8 {
		return 1
	}
	return int32(1 + (8-k)/2)
}

// minEdgeCount is how often a cancel must have been seen to be used.
func minEdgeCount(k int) int32 {
	if k >= 7 {
		return 1
	}
	return int32(1 + (7-k)/2)
}

// gateOpen reports whether every gated technique in flags is allowed at
// these levels.
func (p *Params) gateOpen(flags uint32, l Levels) bool {
	if flags == 0 {
		return true
	}
	for bit, g := range p.Gates {
		if flags&bit != 0 && (l.Execution < g.MinExecution || l.Knowledge < g.MinKnowledge) {
			return false
		}
	}
	return true
}

// completionProb is the chance a route's links all succeed at an execution
// level. Each link's measured human success rate is raised to the noise
// scale: canonical timing at level 8 always completes, the recorded human's
// rate at scale 1, worse below.
func (p *Params) completionProb(links []LinkStat, exec int) float32 {
	ns := float64(p.noiseScale(exec))
	if ns == 0 {
		return 1
	}
	prob := 1.0
	for _, l := range links {
		prob *= math.Pow(float64(l.SuccessRate()), ns)
	}
	return float32(prob)
}

// ambitionExponent turns ambition into risk aversion: at 8 routes are
// valued at expected value, at 1 completion probability is cubed, which
// pushes selection toward short reliable routes without naming any.
func ambitionExponent(amb int) float64 {
	return 1 + 2*float64(8-amb)/7
}

// maxLinks is a coarse cap on route length by ambition, an override for
// cases where expected-value arithmetic still favours something out of
// reach.
func maxLinks(amb int) int {
	return 2 + (amb-1)*2
}
