package main

import "math"

// Client frame rate from the input history. client.dll records one entry per presented frame and sends
// all of a command's frames when there are at most 4 (CCSGOInput::CreateMove). With more, and
// cl_csgo_shoot_trim_input_frames on (the default), it splits the frames' time span into 4 equal buckets
// and keeps the frame closest to each bucket's centre, plus any frame an attack started in. Commands
// with 2-3 entries are therefore consecutive frames, and their gaps are frame times.

type FrameStats struct {
	gaps          []float64 // ms between consecutive entries of commands with 2-3 entries
	EntriesPerCmd [5]int    // 0-3 entries, and 4 or more
}

func (s *FrameStats) Add(frameTimes []float64) {
	s.EntriesPerCmd[min(len(frameTimes), 4)]++
	if len(frameTimes) < 2 || len(frameTimes) > 3 {
		return
	}
	for i := 1; i < len(frameTimes); i++ {
		s.gaps = append(s.gaps, (frameTimes[i]-frameTimes[i-1])*1000/engineFixedTickRate)
	}
}

// Most common frame time in ms, or 0 without data.
func (s *FrameStats) FrameTimeMs() float64 {
	const bin = 0.05
	bins := map[int]int{}
	for _, g := range s.gaps {
		if g > 0.5 && g < 1000/engineFixedTickRate {
			bins[int(g/bin)]++
		}
	}
	best, bestCount := 0, 0
	for b := range bins {
		// Smooth over neighbouring bins, uncapped frame rates jitter.
		if count := bins[b-1] + bins[b] + bins[b+1]; count > bestCount || (count == bestCount && b < best) {
			best, bestCount = b, count
		}
	}
	if bestCount == 0 {
		return 0
	}
	return (float64(best) + 0.5) * bin
}

func (s *FrameStats) FPS() float64 {
	if ft := s.FrameTimeMs(); ft > 0 {
		return 1000 / ft
	}
	return 0
}

// Whether the client ever had more than 4 frames in a command, so that its input history got trimmed.
// A command holds a tick's worth of frames plus the frame carried over from the previous command.
func (s *FrameStats) TrimmingPossible() bool {
	ft := s.FrameTimeMs()
	return ft > 0 && (1000/engineFixedTickRate)/ft+1 > 4
}

func median(values []float64) float64 {
	return percentile(values, 0.5)
}

// Nearest-rank percentile of a sorted slice.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	return sorted[int(p*float64(len(sorted)-1)+0.5)]
}
