package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Writes <demo>_report/ next to the demo:
//
//	summary.csv       one row per player, one verdict per check
//	detections.csv    every detection of the checks ported from cs2kz
//	bhop.csv          jump outcomes around landings, and the ported bhop checks
//	press_height.csv  where in the last 2 units above the floor perfect bhop presses happened
//	subtick.csv       the ported subtick checks, and desubticked jumps
//	inputs.csv        jump button state vs subtick move consistency

const (
	verdictFlagged    = "FLAGGED"
	verdictClean      = "clean"
	verdictNoData     = "not enough data"
	verdictNoCommands = "no commands"
	verdictNoMap      = "no map collision"
)

type check struct {
	name    string
	verdict func(pd *PlayerData, r *reportContext) string
}

type reportContext struct {
	geometryErr error
}

func infractionVerdict(t InfractionType, hasData func(pd *PlayerData) (bool, string)) func(pd *PlayerData, r *reportContext) string {
	return func(pd *PlayerData, r *reportContext) string {
		if n := len(pd.InfractionsOfType(t)); n > 0 {
			return fmt.Sprintf("%s (%d)", verdictFlagged, n)
		}
		if ok, reason := hasData(pd); !ok {
			return reason
		}
		return verdictClean
	}
}

func hasLandings(pd *PlayerData) (bool, string) {
	return pd.NumLandingEvents >= minSampleCount, verdictNoData
}

func hasCommands(pd *PlayerData) (bool, string) {
	return pd.NumCommands > 0, verdictNoCommands
}

var checks = []check{
	{"bhop_hack", infractionVerdict(InfractionBhopHack, hasLandings)},
	{"hyperscroll", infractionVerdict(InfractionHyperscroll, hasLandings)},
	{"invalid_input", infractionVerdict(InfractionInvalidInput, hasCommands)},
	{"subtick_spam", infractionVerdict(InfractionSubtickSpam, hasCommands)},
	{"desubtick", infractionVerdict(InfractionDesubtick, hasCommands)},
	{"press_height", func(pd *PlayerData, r *reportContext) string {
		s := pd.Jumps.PressHeights
		switch {
		case r.geometryErr != nil:
			return verdictNoMap
		case len(s.BandShare) < pressHeightMinSamples:
			return verdictNoData
		case s.Flagged():
			return verdictFlagged
		}
		return verdictClean
	}},
	{"desubticked_jumps", func(pd *PlayerData, r *reportContext) string {
		switch {
		case pd.Jumps.Normal < desubtickedJumpsMinSamples:
			return verdictNoData
		case pd.Jumps.DesubtickedJumpsFlagged():
			return verdictFlagged
		}
		return verdictClean
	}},
	{"input_consistency", func(pd *PlayerData, r *reportContext) string {
		switch {
		case pd.NumCommands == 0:
			return verdictNoCommands
		case pd.Inputs.Flagged():
			return verdictFlagged
		}
		return verdictClean
	}},
}

type reportWriter struct {
	dir     string
	players []*PlayerData
	ctx     *reportContext
}

func writeReport(demoPath string, players []*PlayerData, ctx *reportContext) (string, error) {
	dir := strings.TrimSuffix(demoPath, filepath.Ext(demoPath)) + "_report"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	w := &reportWriter{dir: dir, players: players, ctx: ctx}
	for _, write := range []func() error{w.summary, w.detections, w.bhop, w.pressHeight, w.subtick, w.inputs} {
		if err := write(); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func (w *reportWriter) write(name string, header []string, row func(pd *PlayerData) [][]string) error {
	f, err := os.Create(filepath.Join(w.dir, name))
	if err != nil {
		return err
	}
	defer f.Close()
	// Byte order mark, so spreadsheet programs read player names as UTF-8.
	if _, err := f.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return err
	}
	out := csv.NewWriter(f)
	out.Write(append([]string{"player", "steamid"}, header...))
	for _, pd := range w.players {
		for _, r := range row(pd) {
			out.Write(append([]string{pd.pl.Name, strconv.FormatUint(pd.pl.SteamID64, 10)}, r...))
		}
	}
	out.Flush()
	return out.Error()
}

func itoa(v int) string { return strconv.Itoa(v) }

func ftoa(v float64, decimals int) string {
	if v != v { // NaN
		return ""
	}
	return strconv.FormatFloat(v, 'f', decimals, 64)
}

func share(n, total int) string {
	if total == 0 {
		return ""
	}
	return ftoa(float64(n)/float64(total), 3)
}

func (w *reportWriter) summary() error {
	header := []string{"fps", "flagged_checks"}
	for _, c := range checks {
		header = append(header, c.name)
	}
	return w.write("summary.csv", header, func(pd *PlayerData) [][]string {
		row := []string{ftoa(pd.Frames.FPS(), 0), itoa(len(flaggedChecks(pd, w.ctx)))}
		for _, c := range checks {
			row = append(row, c.verdict(pd, w.ctx))
		}
		return [][]string{row}
	})
}

func flaggedChecks(pd *PlayerData, ctx *reportContext) []string {
	var flagged []string
	for _, c := range checks {
		if strings.HasPrefix(c.verdict(pd, ctx), verdictFlagged) {
			flagged = append(flagged, c.name)
		}
	}
	return flagged
}

var infractionCheckNames = [InfractionCount]string{"bhop_hack", "hyperscroll", "invalid_input", "subtick_spam", "desubtick"}

func (w *reportWriter) detections() error {
	return w.write("detections.csv", []string{"check", "tick", "details"}, func(pd *PlayerData) [][]string {
		var rows [][]string
		for _, inf := range pd.Infractions {
			rows = append(rows, []string{infractionCheckNames[inf.Type], itoa(inf.Tick), inf.Details})
		}
		return rows
	})
}

func (w *reportWriter) bhop() error {
	header := []string{
		"bhop_jumps", "perfect", "in_window", "deferred", "jumpbug_or_ledgegrab", "perfect_rate",
		"scroll_bhops", "scroll_perfect", "key_bhops", "key_perfect", "unknown_input_bhops",
		"landings", "peak_perf_chain", "peak_perf_ratio", "peak_avg_jump_pattern",
		"bhop_hack_detections", "hyperscroll_detections",
	}
	return w.write("bhop.csv", header, func(pd *PlayerData) [][]string {
		j := pd.Jumps
		return [][]string{{
			itoa(j.BhopJumps()), itoa(j.Perfect), itoa(j.InWindow), itoa(j.Deferred), itoa(j.NoLanding), share(j.Perfect, j.BhopJumps()),
			itoa(j.ScrollBhops), itoa(j.ScrollPerfect), itoa(j.KeyBhops), itoa(j.KeyPerfect), itoa(j.UnknownBhops),
			itoa(pd.NumLandingEvents), itoa(pd.PeakPerfChain), ftoa(float64(pd.PeakPerfRatio), 3), ftoa(float64(pd.PeakAveragePattern), 2),
			itoa(len(pd.InfractionsOfType(InfractionBhopHack))), itoa(len(pd.InfractionsOfType(InfractionHyperscroll))),
		}}
	})
}

func (w *reportWriter) pressHeight() error {
	header := []string{
		"measured_presses", "median_height_u", "p10_height_u", "p90_height_u",
		"median_ms_after_2u", "p90_ms_after_2u", "share_in_first_fifth", "outliers", "no_floor_found", "verdict",
	}
	verdict := checkByName("press_height")
	return w.write("press_height.csv", header, func(pd *PlayerData) [][]string {
		s := pd.Jumps.PressHeights
		heights, into := s.Sorted()
		return [][]string{{
			itoa(len(heights)), ftoa(median(heights), 2), ftoa(percentile(heights, 0.1), 2), ftoa(percentile(heights, 0.9), 2),
			ftoa(median(into), 2), ftoa(percentile(into, 0.9), 2), earlyShare(s),
			itoa(s.Outliers), itoa(s.NoFloor), verdict(pd, w.ctx),
		}}
	})
}

func earlyShare(s PressHeightStats) string {
	if len(s.BandShare) == 0 {
		return ""
	}
	return ftoa(s.EarlyShare(), 3)
}

func (w *reportWriter) subtick() error {
	header := []string{
		"commands", "ignored_commands", "commands_with_subtick_moves",
		"invalid_commands", "invalid_input_detections",
		"subtick_spam_commands", "subtick_spam_detections",
		"zero_when_commands", "desubtick_detections",
		"normal_jumps", "normal_jumps_at_tick_start", "tick_start_share", "desubticked_jumps_verdict",
	}
	verdict := checkByName("desubticked_jumps")
	return w.write("subtick.csv", header, func(pd *PlayerData) [][]string {
		return [][]string{{
			itoa(pd.NumCommands), itoa(pd.NumIgnoredCommands), itoa(pd.NumCommandsWithSubtick),
			itoa(pd.NumInvalidCommands), itoa(len(pd.InfractionsOfType(InfractionInvalidInput))),
			itoa(pd.NumSubtickSpamCommands), itoa(len(pd.InfractionsOfType(InfractionSubtickSpam))),
			itoa(pd.NumPossibleDesubtickedCmd), itoa(len(pd.InfractionsOfType(InfractionDesubtick))),
			itoa(pd.Jumps.Normal), itoa(pd.Jumps.NormalAtTickStart), share(pd.Jumps.NormalAtTickStart, pd.Jumps.Normal), verdict(pd, w.ctx),
		}}
	})
}

func (w *reportWriter) inputs() error {
	header := []string{
		"jump_press_moves", "press_moves_without_button_state", "button_changes_without_moves",
		"button_state_chain_breaks", "triple_change_commands", "verdict",
	}
	verdict := checkByName("input_consistency")
	return w.write("inputs.csv", header, func(pd *PlayerData) [][]string {
		s := pd.Inputs
		return [][]string{{
			itoa(s.JumpPressMoves), itoa(s.PressMovesWithoutState), itoa(s.StateChangesWithoutMoves),
			itoa(s.ChainBreaks), itoa(s.TripleChanges), verdict(pd, w.ctx),
		}}
	})
}

func checkByName(name string) func(pd *PlayerData, r *reportContext) string {
	for _, c := range checks {
		if c.name == name {
			return c.verdict
		}
	}
	panic("unknown check " + name)
}

// One line per player for the console.
func printPlayerSummary(pd *PlayerData, ctx *reportContext) {
	status := verdictClean
	if flagged := flaggedChecks(pd, ctx); len(flagged) > 0 {
		status = "FLAGGED: " + strings.Join(flagged, ", ")
	}
	j := pd.Jumps
	heights, _ := j.PressHeights.Sorted()
	pressHeight := "no measured presses"
	if len(heights) > 0 {
		pressHeight = fmt.Sprintf("press height median %.2fu over %d presses", median(heights), len(heights))
	}
	fmt.Printf("%s (%d): %.0f fps, %d bhop jumps (%d perfect), %s, %d/%d normal jumps at tick start -> %s\n",
		pd.pl.Name, pd.pl.SteamID64, pd.Frames.FPS(), j.BhopJumps(), j.Perfect, pressHeight,
		j.NormalAtTickStart, j.Normal, status)
}
