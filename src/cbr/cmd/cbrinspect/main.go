// Command cbrinspect prints a summary of a CBR store: what it holds, which
// commands were reconstructed from AI play, how often played cases failed to
// reproduce, the character AI's direct state changes kept as hypotheses,
// and routes found beyond human precedent.
//
//	go run ./src/cbr/cmd/cbrinspect save/cbr/local/Kung_Fu_Man.cbr
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
}
