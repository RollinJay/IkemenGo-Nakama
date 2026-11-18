package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"runtime/trace"
	"sync"
	"time"
)

// With -profile <folder>, the game writes, for the whole run:
//   - cpu.pprof:  a CPU profile (go tool pprof cpu.pprof)
//   - heap.pprof: the memory in use when the game ends
//   - trace.out:  an execution trace, GC pauses included (go tool trace trace.out)
//   - frames.csv: one line per frame: its time and the garbage collector's
//     cycles and pause time so far
var profiler struct {
	on       bool
	dir      string
	cpu      *os.File
	trace    *os.File
	frames   *os.File
	out      *bufio.Writer
	frame    int64
	last     time.Time
	samples  []metrics.Sample
	stopOnce sync.Once
}

// startProfiling starts the profiles -profile asks for; it does nothing
// without the flag.
func startProfiling(dir string) {
	if dir == "" || dir == "true" {
		dir = "profile"
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(sys.baseDir, dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "-profile: %v\n", err)
		return
	}
	profiler.dir = dir
	open := func(name string) *os.File {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			fmt.Fprintf(os.Stderr, "-profile: %v\n", err)
			return nil
		}
		return f
	}
	if profiler.cpu = open("cpu.pprof"); profiler.cpu != nil {
		if err := pprof.StartCPUProfile(profiler.cpu); err != nil {
			fmt.Fprintf(os.Stderr, "-profile: CPU profile: %v\n", err)
			profiler.cpu.Close()
			profiler.cpu = nil
		}
	}
	if profiler.trace = open("trace.out"); profiler.trace != nil {
		if err := trace.Start(profiler.trace); err != nil {
			fmt.Fprintf(os.Stderr, "-profile: trace: %v\n", err)
			profiler.trace.Close()
			profiler.trace = nil
		}
	}
	if profiler.frames = open("frames.csv"); profiler.frames != nil {
		profiler.out = bufio.NewWriterSize(profiler.frames, 64<<10)
		profiler.out.WriteString("frame,frame_ms,gc_cycles,gc_pause_ms_total\n")
	}
	profiler.samples = []metrics.Sample{
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/sched/pauses/total/gc:seconds"},
	}
	profiler.last = time.Now()
	profiler.on = true
	fmt.Fprintf(os.Stderr, "Profiling into %s\n", dir)
}

// profileFrame notes one frame's time; it is called once per frame.
func profileFrame() {
	if !profiler.on || profiler.out == nil {
		return
	}
	now := time.Now()
	ms := float64(now.Sub(profiler.last)) / float64(time.Millisecond)
	profiler.last = now
	metrics.Read(profiler.samples)
	var cycles uint64
	if profiler.samples[0].Value.Kind() == metrics.KindUint64 {
		cycles = profiler.samples[0].Value.Uint64()
	}
	pauseMs := 0.0
	if profiler.samples[1].Value.Kind() == metrics.KindFloat64Histogram {
		pauseMs = histogramSum(profiler.samples[1].Value.Float64Histogram()) * 1000
	}
	profiler.frame++
	fmt.Fprintf(profiler.out, "%d,%.3f,%d,%.3f\n", profiler.frame, ms, cycles, pauseMs)
}

// histogramSum estimates a histogram's total from its buckets' midpoints.
func histogramSum(h *metrics.Float64Histogram) float64 {
	sum := 0.0
	for i, n := range h.Counts {
		if n == 0 {
			continue
		}
		lo, hi := h.Buckets[i], h.Buckets[i+1]
		switch {
		case lo < -1e300:
			lo = hi
		case hi > 1e300:
			hi = lo
		}
		sum += float64(n) * (lo + hi) / 2
	}
	return sum
}

// stopProfiling finishes and closes every profile; later calls do nothing.
func stopProfiling() {
	if !profiler.on {
		return
	}
	profiler.stopOnce.Do(func() {
		if profiler.cpu != nil {
			pprof.StopCPUProfile()
			profiler.cpu.Close()
		}
		if profiler.trace != nil {
			trace.Stop()
			profiler.trace.Close()
		}
		if profiler.out != nil {
			profiler.out.Flush()
			profiler.frames.Close()
		}
		if f, err := os.Create(filepath.Join(profiler.dir, "heap.pprof")); err == nil {
			runtime.GC() // the memory still in use, not garbage
			pprof.WriteHeapProfile(f)
			f.Close()
		}
		profiler.on = false
	})
}
