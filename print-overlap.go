package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	demoinfocs "github.com/markus-wa/demoinfocs-golang/v6/pkg/demoinfocs"
	common "github.com/markus-wa/demoinfocs-golang/v6/pkg/demoinfocs/common"
	events "github.com/markus-wa/demoinfocs-golang/v6/pkg/demoinfocs/events"
	msg "github.com/markus-wa/demoinfocs-golang/v6/pkg/demoinfocs/msg"
)

// Run like this: go run . -demo="/path/to/demo.dem"
// Run like this: go run . -dir="/path/to/"
type Result struct {
	Path  string
	Error error
}

func main() {

	dir := flag.String("dir", "", "Directory to process")

	demo := flag.String("demo", "", "Demo file `path`")

	verbose := flag.Bool("v", false, "Enable verbose stdout (prints every detection)")
	debugPlayer := flag.String("debug-player", "", "Player name or SteamID64 to dump landing events and suspicious commands for")
	csgoDir := flag.String("csgo", "", "Path to CS2's game/csgo folder, to read map collision from. Found automatically when empty")
	noMap := flag.Bool("no-map", false, "Skip the checks that need map collision")

	max := flag.Int("max-concurrent", 8, "Maximum amount of demos parsed at the same time")

	result := make(chan Result)

	// Parse the flags
	flag.Parse()

	// WaitGroup to wait for all goroutines to finish
	wg := &sync.WaitGroup{}
	semaphore := make(chan struct{}, *max)

	if (*dir == "" && *demo == "") || (*dir != "" && *demo != "") {
		fmt.Println("Error: -dir OR -demo flag is required")
		flag.Usage()
		os.Exit(1)
	}

	var geometry *GeometryProvider
	if !*noMap {
		geometry = NewGeometryProvider(*csgoDir)
	}

	fmt.Println("Demo Anticheat Parser by zer0.k")
	fmt.Println("----")

	if *demo != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()

			parseDemo(*demo, *verbose, *debugPlayer, geometry, result)
		}()

	} else {
		fmt.Println("Parsing dir", *dir)

		err := filepath.Walk(*dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			if !info.IsDir() && filepath.Ext(info.Name()) == ".dem" {
				fmt.Println("Parsing demo file:", path)
				wg.Add(1)
				go func(path string, verbose bool) {
					defer wg.Done()
					defer func() { <-semaphore }() // Release the semaphore
					semaphore <- struct{}{}        // Acquire semaphore
					parseDemo(path, verbose, *debugPlayer, geometry, result)
				}(path, *verbose)
			}

			return nil
		})
		checkError(err)
	}

	go func() {
		wg.Wait()
		close(result)
	}()

	for res := range result {
		if res.Error != nil {
			fmt.Printf("Failed goroutines: Path=%s, Error: %v\n", res.Path, res.Error)
		}
	}

	fmt.Println("Parsing done.")
}

func parseDemo(path string, verbose bool, debugPlayer string, geometryProvider *GeometryProvider, result chan<- Result) {
	reported := false
	mapPlayerData := make(map[uint64]*PlayerData)

	var res Result
	res.Path = path
	defer func() {
		if err := recover(); err != nil {
			fmt.Printf("panic occurred for path %s: %s\n", path, err)
			res.Error = fmt.Errorf("panic occurred: %v", err)
			result <- res
		}
	}()

	f, err := os.Open(path)
	checkError(err)

	defer f.Close()

	cfg := demoinfocs.DefaultParserConfig
	cfg.UserCmdParsing = demoinfocs.UserCmdParsingFull
	p := demoinfocs.NewParserWithConfig(f, cfg)
	defer p.Close()

	report := &reportContext{geometryErr: fmt.Errorf("map collision disabled")}
	var geometry *Geometry
	p.RegisterNetMessageHandler(func(m *msg.CSVCMsg_ServerInfo) {
		if geometryProvider == nil || geometry != nil {
			return
		}
		geometry, report.geometryErr = geometryProvider.Get(m.GetMapName(), path)
		if report.geometryErr != nil {
			fmt.Printf("%s: press height check disabled: %v\n", path, report.geometryErr)
		}
	})

	getPlayerData := func(player *common.Player) *PlayerData {
		if player == nil || player.SteamID64 == 0 || player.IsBot {
			return nil
		}
		pd := mapPlayerData[player.SteamID64]
		if pd == nil {
			pd = &PlayerData{pl: player}
			pd.pawn.lastValidMoveTypeTick = -1
			pd.pawn.lastTeleportTick = -1
			mapPlayerData[player.SteamID64] = pd
		}
		// The player object can change on reconnect.
		pd.pl = player
		return pd
	}

	getPlayerDataFromSlot := func(playerSlot int32) *PlayerData {
		if playerSlot < 0 {
			return nil
		}
		for _, player := range p.GameState().Participants().All() {
			if player == nil || player.SteamID64 == 0 {
				continue
			}
			if int32(player.EntityID-1) == playerSlot {
				return getPlayerData(player)
			}
		}
		return nil
	}

	newContext := func(pd *PlayerData) *detectionContext {
		return &detectionContext{
			tick:     p.GameState().IngameTick(),
			verbose:  verbose || shouldDumpPlayer(debugPlayer, pd.pl),
			debug:    shouldDumpPlayer(debugPlayer, pd.pl),
			geometry: geometry,
		}
	}

	// demoinfocs reconstructs every command from the full and delta-encoded svc_UserCmds payloads.
	p.RegisterEventHandler(func(e events.UserCmd) {
		if reported {
			return
		}
		pd := getPlayerData(e.Player)
		if pd == nil {
			pd = getPlayerDataFromSlot(e.PlayerSlot)
		}
		if pd == nil || !pd.pl.IsAlive() {
			return
		}
		cmd := baseUserCmdFromProto(e.Command, e.ServerTickExecuted)
		ctx := newContext(pd)
		if ctx.debug {
			dumpSuspiciousCommand(pd, e.CommandNumber, &cmd, ctx.tick)
		}
		pd.OnUserCmd(e.CommandNumber, cmd, ctx)
	})

	userCmdWarnings := 0
	p.RegisterEventHandler(func(e events.ParserWarn) {
		switch e.Type {
		case events.WarnTypeUserCommandDeltaDecodeFailed, events.WarnTypeUserCommandBaselineMissing, events.WarnTypeUserCommandBaselineMismatch:
			userCmdWarnings++
		}
	})

	p.RegisterEventHandler(func(events.FrameDone) {
		if reported {
			return
		}
		for _, pl := range p.GameState().Participants().Playing() {
			pd := getPlayerData(pl)
			if pd == nil {
				continue
			}
			ctx := newContext(pd)
			numLandings, numPerfs := pd.NumLandingEvents, pd.NumPerfectBhops
			pd.OnTick(ctx.tick, pl.PlayerPawnEntity(), ctx)
			if ctx.debug && (pd.NumLandingEvents != numLandings || pd.NumPerfectBhops != numPerfs) {
				fmt.Printf("  tick=%d cmd=%d landings=%d perfs=%d window=%s\n", ctx.tick, pd.currentCmdNum, pd.NumLandingEvents, pd.NumPerfectBhops, pd.landingEventsString())
			}
		}
	})

	spewReport := func() {
		if reported {
			return
		}
		reported = true
		// One entry per player, in the order they appear in the demo.
		var players []*PlayerData
		seen := make(map[uint64]bool)
		for _, pl := range p.GameState().Participants().All() {
			if pd := mapPlayerData[pl.SteamID64]; pd != nil && !seen[pl.SteamID64] {
				seen[pl.SteamID64] = true
				players = append(players, pd)
			}
		}
		dir, err := writeReport(path, players, report)
		checkError(err)
		if verbose {
			fmt.Printf("Game duration: %d ticks (%f minutes), %d user command decode warnings\n", p.GameState().IngameTick(),
				float64(p.GameState().IngameTick())/64.0/60.0, userCmdWarnings)
			for _, pd := range players {
				printPlayerSummary(pd, report)
			}
		}
		fmt.Println("Report written to", dir)
	}
	p.RegisterEventHandler(func(events.AnnouncementWinPanelMatch) {
		spewReport()
	})
	// Parse to end
	err = p.ParseToEnd()
	spewReport()
	fmt.Println("Parsing done for", path)
	checkError(err)

}

func checkError(err error) {
	if err != nil {
		panic(err)
	}
}

func shouldDumpPlayer(debugPlayer string, player *common.Player) bool {
	if debugPlayer == "" || player == nil {
		return false
	}
	if player.Name == debugPlayer {
		return true
	}
	return fmt.Sprintf("%d", player.SteamID64) == debugPlayer
}

// Prints commands that feed into the subtick detectors, before they are processed.
func dumpSuspiciousCommand(pd *PlayerData, cmdNum int32, cmd *BaseUserCmd, tick int) {
	reasons := []string{}
	if !verifyCommand(cmd) {
		reasons = append(reasons, "invalid")
	}
	if hasExcessiveSubtickMovesWithAngles(cmd) {
		reasons = append(reasons, "subtick_spam")
	}
	if possibleDesubtickedCommand(cmd, pd.movementImpulses) {
		reasons = append(reasons, "desubtick")
	}
	if len(reasons) == 0 {
		return
	}
	fmt.Printf("  tick=%d cmd=%d [%s] buttons=(0x%x,0x%x,0x%x) moves=%d\n", tick, cmdNum, strings.Join(reasons, ","),
		cmd.Buttons.ButtonState1, cmd.Buttons.ButtonState2, cmd.Buttons.ButtonState3, len(cmd.SubtickMoves))
	for _, step := range cmd.SubtickMoves {
		fmt.Printf("    button=0x%x pressed=%t when=%.4f fwd=%g left=%g pitch=%g yaw=%g\n", step.Button, step.Pressed, step.When,
			step.AnalogForwardDelta, step.AnalogLeftDelta, step.PitchDelta, step.YawDelta)
	}
}
