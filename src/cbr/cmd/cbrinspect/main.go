// Command cbrinspect prints a summary of a CBR store: what it holds, which
// commands were reconstructed from AI play, how often played cases failed to
// reproduce, the character AI's direct state changes kept as hypotheses,
// whether each command was found to work where the character AI's direct
// state changes used it (verdicts by spot: the engine's checks at the
// moment of each change, and the CBR layer's test plays), routes found
// beyond human precedent, the character's own AI's record by
// situation (the baseline the CBR layer must beat to take over), its record
// where each move was on offer, and what the CBR layer's own plays of each
// move achieved, by the situation they were played in.
//
//	go run ./src/cbr/cmd/cbrinspect save/cbr/local/kfm-16a05657.cbr
//
// A store's file name is the character's definition file name without its
// extension, in lower case, with characters other than letters, digits, '-'
// and '_' replaced by '_'; then '-' and a hash (32-bit FNV-1a, in
// hexadecimal) of that file's path in lower case. The example is the store
// of chars/kfm/kfm.def.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/ikemen-engine/Ikemen-GO/src/cbr"
)

var sourceNames = map[cbr.Source]string{
	cbr.SrcUnknown: "unknown", cbr.SrcHumanLocal: "human", cbr.SrcHumanLobby: "lobby",
	cbr.SrcRanked: "ranked", cbr.SrcRankedTop: "ranked_top", cbr.SrcTrial: "trial",
	cbr.SrcAuthoredAI: "authored", cbr.SrcEngineAI: "engine", cbr.SrcSynthesized: "hypothesis",
	cbr.SrcCharacterAI: "character AI",
}

type count struct {
	key string
	n   int
}

func top(m map[string]int, n int) []count {
	var out []count
	for k, v := range m {
		out = append(out, count{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].key < out[j].key
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

func main() {
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: cbrinspect store.cbr")
		os.Exit(2)
	}
	st, err := cbr.LoadStore(flag.Arg(0), "")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	p := cbr.DefaultParams()
	sources, results, assisted, moves := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	var command, movement, novel, loops int
	var plays, execFails int32
	for _, c := range st.Cases {
		sources[sourceNames[c.Source]]++
		results[c.Completion.Result.String()]++
		if c.Kind == cbr.KindCommand {
			command++
			moves[fmt.Sprint(c.MoveRef)]++
		} else {
			movement++
		}
		if c.Command != "" {
			assisted[c.Command]++
		}
		if c.Technique.Novel {
			novel++
		}
		if c.Technique.Flags&cbr.TechLoop != 0 {
			loops++
		}
		plays += c.Outcome.Plays
		execFails += c.Outcome.ExecFails
	}
	fmt.Printf("character %q: %d cases (%d command, %d movement)\n", st.Char, st.Len(), command, movement)
	fmt.Printf("sources: %v\n", top(sources, 0))
	fmt.Printf("chain results: %v\n", top(results, 0))
	fmt.Printf("most recorded moves (state): %v\n", top(moves, 12))
	fmt.Printf("commands reconstructed from AI play: %v\n", top(assisted, 0))
	fmt.Printf("played by the CBR layer: %d plays, %d where the command did not come out\n", plays, execFails)
	fmt.Printf("models: %d reach entries, %d cancel edges, %d routes\n", len(st.Reach.Bins), len(st.Trans.Edges), len(st.Routes))
	fmt.Printf("cases whose route has no human precedent: %d (loops: %d)\n", novel, loops)
	fmt.Printf("character AI changes states directly: %v\n", st.ChangeStateAI)
	if len(st.Hypotheses) > 0 {
		var states []int32
		for s := range st.Hypotheses {
			states = append(states, s)
		}
		sort.Slice(states, func(i, j int) bool { return states[i] < states[j] })
		fmt.Println("hypotheses (states the AI entered with no command):")
		for _, s := range states {
			h := st.Hypotheses[s]
			status := "untested"
			switch {
			case h.CheatOnly:
				status = "only reachable by direct state change"
			case h.Successes > 0:
				status = "reachable with inputs"
			case h.Command == "":
				status = "no command known for it yet"
			}
			fmt.Printf("  state %d: seen %d, command %q, played %d, came out %d: %s\n", s, h.Seen, h.Command, h.Attempts, h.Successes, status)
		}
	}
	// Whether each command works where the character AI's direct state
	// changes used it, spot by spot: at the moment of each change the engine
	// checked whether a person's command would have made it, and the CBR
	// layer's plays tested the command's inputs.
	if len(st.Verdicts) > 0 {
		keys := make([]cbr.VerdictKey, 0, len(st.Verdicts))
		for k := range st.Verdicts {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			a, b := keys[i], keys[j]
			if a.State != b.State {
				return a.State < b.State
			}
			if a.Command != b.Command {
				return a.Command < b.Command
			}
			if a.From != b.From {
				return a.From < b.From
			}
			return a.Stance < b.Stance
		})
		fmt.Printf("commands by spot (%d):\n", len(keys))
		for i, k := range keys {
			if i == 40 {
				fmt.Printf("  ... %d more\n", len(keys)-40)
				break
			}
			v := st.Verdicts[k]
			from := "with control"
			if k.From >= 0 {
				from = fmt.Sprintf("from state %d", k.From)
			}
			fmt.Printf("  state %d by %q, %s, %s: %s\n", k.State, k.Command, from, stance(k.Stance), verdictLine(v))
		}
	}
	// Best-valued command cases, as a quick look at what the store prefers.
	var cmds []*cbr.Case
	for _, c := range st.Cases {
		if c.Kind == cbr.KindCommand {
			cmds = append(cmds, c)
		}
	}
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Value(p) > cmds[j].Value(p) })
	if len(cmds) > 8 {
		cmds = cmds[:8]
	}
	fmt.Println("highest-valued command cases:")
	for _, c := range cmds {
		fmt.Printf("  state %d from %s, value %.2f over %.1f trials, chain result %s, damage %.0f%%\n",
			c.MoveRef, sourceNames[c.Source], c.Value(p), c.Outcome.Trials, c.Completion.Result, 100*c.Completion.DamageDealt)
	}

	// Results are chain rewards, a fraction of a life bar per exchange, each
	// decision credited with its share of its exchange's reward; the share
	// shown is the average one in the situation, by which the take-over
	// margin is scaled there.
	const show = 20
	baseKeys := make([]uint64, 0, len(st.Baseline))
	for k := range st.Baseline {
		baseKeys = append(baseKeys, k)
	}
	sort.Slice(baseKeys, func(i, j int) bool {
		a, b := st.Baseline[baseKeys[i]], st.Baseline[baseKeys[j]]
		if a.Trials != b.Trials {
			return a.Trials > b.Trials
		}
		return baseKeys[i] < baseKeys[j]
	})
	fmt.Printf("character's own AI, by situation (%d records, most results first):\n", len(baseKeys))
	for i, k := range baseKeys {
		if i == show {
			fmt.Printf("  ... %d more\n", len(baseKeys)-show)
			break
		}
		b := st.Baseline[k]
		fmt.Printf("  %s: %.1f results, mean %+.3f, share %.2f\n", cbr.SituationString(k), b.Trials, b.ValueSum/b.Trials, b.Shares/b.Trials)
	}
	// The base AI's results where a move was on offer, by situation and
	// move: what a move is compared with once there are enough of them.
	offered := make([]uint64, 0, len(st.Offered))
	for k := range st.Offered {
		offered = append(offered, k)
	}
	sort.Slice(offered, func(i, j int) bool {
		a, b := st.Offered[offered[i]], st.Offered[offered[j]]
		if a.Trials != b.Trials {
			return a.Trials > b.Trials
		}
		return offered[i] < offered[j]
	})
	fmt.Printf("character's own AI where a move was on offer (%d records, most results first):\n", len(offered))
	for i, k := range offered {
		if i == show {
			fmt.Printf("  ... %d more\n", len(offered)-show)
			break
		}
		b := st.Offered[k]
		fmt.Printf("  %s: %.1f results, mean %+.3f, share %.2f\n", cbr.PlayString(k), b.Trials, b.ValueSum/b.Trials, b.Shares/b.Trials)
	}
	// The CBR layer's own plays, by move and the situation they were played
	// in: what each move has proved against the character's own AI there.
	var played []uint64
	for k, a := range st.Played {
		if a.Trials > 0 || a.Contests > 0 {
			played = append(played, k)
		}
	}
	sort.Slice(played, func(i, j int) bool {
		a, b := st.Played[played[i]], st.Played[played[j]]
		if a.Trials != b.Trials {
			return a.Trials > b.Trials
		}
		return played[i] < played[j]
	})
	fmt.Printf("moves the CBR layer played, by situation (%d, most plays first):\n", len(played))
	for i, k := range played {
		if i == show {
			fmt.Printf("  ... %d more\n", len(played)-show)
			break
		}
		a := st.Played[k]
		mean := ""
		if a.Trials > 0 {
			mean = fmt.Sprintf(", mean %+.3f, %.1f exchanges' worth", a.Sum/a.Trials, a.Shares)
		}
		contests := ""
		if a.Contests > 0 {
			contests = fmt.Sprintf("; the character's own AI took over from %.1f plays", a.Contests)
		}
		fmt.Printf("  %s: %.1f plays%s%s\n", cbr.PlayString(k), a.Trials, mean, contests)
	}
}

func stance(st cbr.StateType) string {
	switch st {
	case cbr.StStand:
		return "standing"
	case cbr.StCrouch:
		return "crouching"
	case cbr.StAir:
		return "in the air"
	case cbr.StLying:
		return "lying"
	}
	return "unknown stance"
}

// verdictLine describes what is known of a command in a spot.
func verdictLine(v *cbr.Verdict) string {
	s := ""
	if v.Reachable > 0 || v.Cheat > 0 {
		s = fmt.Sprintf("the character AI's changes: %d a person's command could have made", v.Reachable)
		if v.Reachable > 0 {
			s += fmt.Sprintf(" (from power %d)", v.ReachPower)
		}
		s += fmt.Sprintf(", %d none could; ", v.Cheat)
	}
	s += fmt.Sprintf("the CBR layer's plays: %d came out", v.Successes)
	if v.Successes > 0 {
		s += fmt.Sprintf(" (from power %d)", v.OKPower)
	}
	s += fmt.Sprintf(", %d failed", v.Fails)
	if n := min(int(v.Fails+v.Cheat), 3); n == 3 {
		s += fmt.Sprintf("; found not to work up to power %d", v.FailTop[2])
	}
	return s
}
