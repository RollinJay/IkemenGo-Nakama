package cbr

// distance is the weighted dissimilarity between the current state and a
// case's start state. Each metric is normalised to [0,1] and multiplied by
// its weight; the result is a raw weighted sum, the scale the recovered
// thresholds were tuned on. Round state is a hard filter applied before this
// is called, not a term in it.
func (p *Params) distance(cur, cas *Feat) float32 {
	w := &p.W
	var d float32

	// Self against the enemy.
	if cur.Enemy.Present && cas.Enemy.Present {
		d += w.RelX * norm(cur.Enemy.RelX-cas.Enemy.RelX, p.MaxXPos)
		d += w.RelY * 0.5 * (norm(cur.Enemy.RelY-cas.Enemy.RelY, p.MaxYPos) + norm(cur.Self.RelY-cas.Self.RelY, p.MaxYPos))
	} else if cur.Enemy.Present != cas.Enemy.Present {
		d += w.RelX + w.RelY
	}
	d += w.VelX * norm(cur.Self.VelX-cas.Self.VelX, p.MaxVel)
	d += w.VelY * norm(cur.Self.VelY-cas.Self.VelY, p.MaxVel)
	d += w.InputDir * dirHistDiff(cur.InDir, cas.InDir)
	d += w.InputBtn * btnHistDiff(cur.InBtn, cas.InBtn)
	d += charStateDiff(p, &cur.Self, &cas.Self, w.Airborne, w.Lying, w.Hit, w.Block, w.Attack, w.MoveID)
	if cur.Wall != cas.Wall {
		d += w.NearWall
	}
	if cur.Pressure != cas.Pressure {
		d += w.PressureMoveID
	} else if cur.Pressure && cur.Self.StateNo != cas.Self.StateNo {
		d += w.PressureMoveID
	}
	if cur.GotHit != cas.GotHit {
		d += w.GetHit
	}
	if cur.DidHit != cas.DidHit {
		d += w.DidHit
	}
	d += w.FrameAdv * norm(float32(cur.FrameAdv-cas.FrameAdv), 20)
	if cur.Initiator != cas.Initiator {
		d += w.FrameAdvInitiator
	}
	d += w.ComboSimilarity * norm(float32(cur.ComboMoves-cas.ComboMoves), p.ComboLength)
	d += w.ObjectOrder * orderDiff(cur.Order, cur.OrderLen, cas.Order, cas.OrderLen)

	// Enemy.
	if cur.Enemy.Present && cas.Enemy.Present {
		e, c := &cur.Enemy, &cas.Enemy
		d += w.EnemyVelX * norm(e.VelX-c.VelX, p.MaxVel)
		d += w.EnemyVelY * norm(e.VelY-c.VelY, p.MaxVel)
		d += charStateDiff(p, e, c, w.EnemyAirborne, w.EnemyLying, w.EnemyHit, w.EnemyBlock, w.EnemyAttack, w.EnemyMoveID)
		// Enemy pressure: the enemy attacking while the focus guards.
		ep, cp := cur.Self.BlockStun > 0 && e.Move == MtAttack, cas.Self.BlockStun > 0 && c.Move == MtAttack
		if ep != cp || (ep && e.StateNo != c.StateNo) {
			d += w.EnemyPressureMoveID
		}
	}

	// Helpers, enemy helpers and allies, each paired by best match.
	d += groupDiff(p, cur.SelfHelpers, cas.SelfHelpers, w.HelperRelX, w.HelperRelY, w.HelperVelX, w.HelperVelY, 0, 0, 0, 0, 0)
	d += groupDiff(p, cur.EnemyHelpers, cas.EnemyHelpers, w.EnemyHelperRelX, w.EnemyHelperRelY, w.EnemyHelperVelX, w.EnemyHelperVelY, 0, 0, 0, 0, 0)
	d += groupDiff(p, cur.Allies, cas.Allies, w.AllyRelX, w.AllyRelY, w.AllyVelX, w.AllyVelY, w.AllyAirborne, w.AllyAttack, w.AllyHit, w.AllyBlock, w.AllyMoveID)

	// Round context.
	switch {
	case cur.TimeFrac >= 0 && cas.TimeFrac >= 0:
		d += w.TimeFrac * absf(cur.TimeFrac-cas.TimeFrac)
	case (cur.TimeFrac >= 0) != (cas.TimeFrac >= 0):
		d += w.TimeFrac * 0.5
	}
	d += w.LifeLead * norm(cur.LifeLead-cas.LifeLead, 2)
	d += w.Power * absf(cur.Self.PowerFrac-cas.Self.PowerFrac)

	d += varDiff(p.Vars, cur.Vars, cas.Vars)
	return d
}

// beingHit mirrors CharSnap.BeingHit: guard-hit states share move type H
// in MUGEN, so guarding is not counted as being hit.
func (c *CharFeat) beingHit() bool {
	return c.HitStun > 0 || (c.Move == MtHit && c.BlockStun == 0 && c.State != StLying && !c.Ctrl)
}

func norm(v, scale float32) float32 {
	if scale <= 0 {
		return 0
	}
	return clampf(absf(v)/scale, 0, 1)
}

// charStateDiff compares state flags of two characters. Stun and attack
// progress refine the comparison when both sides share the state.
func charStateDiff(p *Params, a, b *CharFeat, wAir, wLie, wHit, wBlock, wAtk, wMove float32) float32 {
	var d float32
	if (a.State == StAir) != (b.State == StAir) {
		d += wAir
	}
	if (a.State == StLying) != (b.State == StLying) {
		d += wLie
	}
	ah, bh := a.beingHit(), b.beingHit()
	switch {
	case ah != bh:
		d += wHit
	case ah:
		d += wHit * norm(float32(a.HitStun-b.HitStun), float32(p.MaxHitstunDiff))
	}
	ab, bb := a.BlockStun > 0, b.BlockStun > 0
	switch {
	case ab != bb:
		d += wBlock
	case ab:
		d += wBlock * norm(float32(a.BlockStun-b.BlockStun), float32(p.MaxBlockstunDiff))
	}
	aa, ba := a.Move == MtAttack, b.Move == MtAttack
	switch {
	case aa != ba:
		d += wAtk
	case aa:
		d += wAtk * norm(float32(a.StateTime-b.StateTime), float32(p.MaxAttackStateDiff))
		if a.StateNo != b.StateNo {
			d += wMove
		}
	}
	return d
}

// groupDiff pairs two sets of characters greedily by position and averages
// the paired cost; unpaired members cost the full group weight.
func groupDiff(p *Params, cur, cas []CharFeat, wx, wy, wvx, wvy, wAir, wAtk, wHit, wBlock, wMove float32) float32 {
	if len(cur) == 0 && len(cas) == 0 {
		return 0
	}
	full := wx + wy + wvx + wvy + wAir + wAtk + wHit + wBlock + wMove
	n := len(cur)
	if len(cas) > n {
		n = len(cas)
	}
	used := make([]bool, len(cas))
	var total float32
	matched := 0
	for i := range cur {
		best, bi := float32(-1), -1
		for j := range cas {
			if used[j] {
				continue
			}
			dd := norm(cur[i].RelX-cas[j].RelX, p.MaxXPos) + norm(cur[i].RelY-cas[j].RelY, p.MaxYPos)
			if best < 0 || dd < best {
				best, bi = dd, j
			}
		}
		if bi < 0 {
			continue
		}
		used[bi] = true
		matched++
		a, b := &cur[i], &cas[bi]
		c := wx*norm(a.RelX-b.RelX, p.MaxXPos) + wy*norm(a.RelY-b.RelY, p.MaxYPos) +
			wvx*norm(a.VelX-b.VelX, p.MaxVel) + wvy*norm(a.VelY-b.VelY, p.MaxVel)
		if wAir+wAtk+wHit+wBlock+wMove > 0 {
			c += charStateDiff(p, a, b, wAir, 0, wHit, wBlock, wAtk, wMove)
		}
		total += c
	}
	total += float32(n-matched) * full
	return total / float32(n)
}

func dirHistDiff(a, b []uint8) float32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return 0
	}
	oa, ob := len(a)-n, len(b)-n
	diff := 0
	for i := 0; i < n; i++ {
		if a[oa+i] != b[ob+i] {
			diff++
		}
	}
	return float32(diff) / float32(n)
}

func btnHistDiff(a, b []InputBits) float32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return 0
	}
	oa, ob := len(a)-n, len(b)-n
	diff := 0
	for i := 0; i < n; i++ {
		if a[oa+i] != b[ob+i] {
			diff++
		}
	}
	return float32(diff) / float32(n)
}

func orderDiff(a uint64, al uint8, b uint64, bl uint8) float32 {
	n := al
	if bl > n {
		n = bl
	}
	if n == 0 {
		return 0
	}
	diff := 0
	for i := uint8(0); i < n; i++ {
		if (a>>(3*uint(i)))&7 != (b>>(3*uint(i)))&7 {
			diff++
		}
	}
	return float32(diff) / float32(n)
}

func varDiff(decls []VarDecl, a, b []float32) float32 {
	var d float32
	for i, decl := range decls {
		if i >= len(a) || i >= len(b) {
			break
		}
		diff := absf(a[i] - b[i])
		if diff <= decl.Tolerance {
			continue
		}
		r := decl.Range
		if r <= 0 {
			r = 1
		}
		d += decl.MaxCost * clampf((diff-decl.Tolerance)/r, 0, 1)
	}
	return d
}
