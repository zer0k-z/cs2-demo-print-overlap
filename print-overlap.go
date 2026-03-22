package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	demoinfocs "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs"
	common "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
	events "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
	st "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/sendtables"
)

type MoveData struct {
	pl                          *common.Player
	Tracking                    bool
	DebugKeepAllSubtickEvents   bool
	HasSubtickData              bool
	SubtickDecodeErrors         int
	SubtickButtons              uint64
	RecentForwardBackwardEvents []InputEvent
	RecentLeftRightEvents       []InputEvent
	LastWSOverlapTick           int
	LastADOverlapTick           int
	WSOverlapTicks              []int
	ADOverlapTicks              []int
	IsWSOverlapping             bool
	IsADOverlapping             bool
	LastMoveAttemptTick         int
	NumMoveTicks                int
	GoodGroundSwitchCount       int
	OldButtons                  uint64
	OldYaw                      float32
	TurnState                   int8
	GoodTurns                   uint32
	AirTime                     uint32
	AirTurnData                 []float64
}

const (
	IN_FORWARD   = uint64(0x8)
	IN_BACK      = uint64(0x10)
	IN_MOVELEFT  = uint64(0x200)
	IN_MOVERIGHT = uint64(0x400)
)

func (mv MoveData) GetWSTotalOverlap() int {
	total := 0
	for _, v := range mv.WSOverlapTicks {
		total += v
	}
	return total
}

func (mv MoveData) GetADTotalOverlap() int {
	total := 0
	for _, v := range mv.ADOverlapTicks {
		total += v
	}
	return total
}

func (mv MoveData) GetTurning() int8 {
	curYaw := mv.pl.ViewDirectionY()
	turning := curYaw != mv.OldYaw
	if !turning {
		return 0
	}
	if curYaw < mv.OldYaw-180 || (curYaw > mv.OldYaw && curYaw < mv.OldYaw+180) {
		return -1
	}
	return 1
}

func (mv MoveData) checkGoodSwitch(newButtons uint64) bool {
	if mv.OldButtons&IN_FORWARD != 0 && // Used to press forward
		newButtons&IN_FORWARD == 0 && // Not anymore
		mv.OldButtons&IN_BACK == 0 && // Did not press backward
		newButtons&IN_BACK != 0 { // Now do though
		return true
	}
	if mv.OldButtons&IN_MOVELEFT != 0 &&
		newButtons&IN_MOVELEFT == 0 &&
		mv.OldButtons&IN_MOVERIGHT == 0 &&
		newButtons&IN_MOVERIGHT != 0 {
		return true
	}
	if mv.OldButtons&IN_BACK != 0 &&
		newButtons&IN_BACK == 0 &&
		mv.OldButtons&IN_FORWARD == 0 &&
		newButtons&IN_FORWARD != 0 {
		return true
	}
	if mv.OldButtons&IN_MOVERIGHT != 0 &&
		newButtons&IN_MOVERIGHT == 0 &&
		mv.OldButtons&IN_MOVELEFT == 0 &&
		newButtons&IN_MOVELEFT != 0 {
		return true
	}
	return false
}

// Run like this: go run print-overlap.go -demo="/path/to/demo.dem"
// Run like this: go run print-overlap.go -dir="/path/to/"
type Result struct {
	Path  string
	Error error
}

func main() {

	dir := flag.String("dir", "", "Directory to process")

	demo := flag.String("demo", "", "Demo file `path`")

	verbose := flag.Bool("v", false, "Enable verbose stdout")
	debugPlayer := flag.String("debug-player", "", "Player name or SteamID64 to dump subtick left/right events for")

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

	fmt.Println("Movement Input Parser by zer0.k")
	fmt.Println("Subtick null-detection uses decoded usercmd subtick move data when available.")
	fmt.Println("----")

	if *demo != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()

			parseDemo(*demo, *verbose, *debugPlayer, result)
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
					parseDemo(path, verbose, *debugPlayer, result)
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

func parseDemo(path string, verbose bool, debugPlayer string, result chan<- Result) {
	reported := false
	mapPlayerEx := make(map[uint64]*MoveData)
	outputPath := strings.TrimSuffix(path, filepath.Ext(path)) + ".csv"

	var res Result
	res.Path = path
	defer func() {
		if err := recover(); err != nil {
			fmt.Printf("panic occurred for path %s: %s\n", path, err)
			os.Remove(outputPath)
			res.Error = fmt.Errorf("panic occurred: %v", err)
			result <- res
		}
	}()

	output, err := os.Create(outputPath)
	checkError(err)

	defer output.Close()

	f, err := os.Open(path)
	checkError(err)

	defer f.Close()

	stat, err := f.Stat()
	checkError(err)

	p := demoinfocs.NewParser(f)
	defer p.Close()

	getMoveDataFromPlayer := func(player *common.Player) *MoveData {
		if player == nil || player.SteamID64 == 0 {
			return nil
		}
		if mapPlayerEx[player.SteamID64] == nil {
			mapPlayerEx[player.SteamID64] = &MoveData{pl: player, Tracking: true}
		}
		if shouldDumpPlayer(debugPlayer, player) {
			mapPlayerEx[player.SteamID64].DebugKeepAllSubtickEvents = true
		}
		return mapPlayerEx[player.SteamID64]
	}

	getMoveDataFromPlayerSlot := func(playerSlot int32) *MoveData {
		if playerSlot < 0 {
			return nil
		}
		for _, player := range p.GameState().Participants().All() {
			if player == nil || player.SteamID64 == 0 {
				continue
			}
			if int32(player.EntityID-1) == playerSlot {
				return getMoveDataFromPlayer(player)
			}
		}
		return nil
	}

	registerSubtickUserCmdHandler(p, getMoveDataFromPlayerSlot)

	// Do not use this at the end of the game.
	getOverlapDataFromPawnEntity := func(pawnEntity st.Entity) *MoveData {
		controllerProp, hasProp := pawnEntity.PropertyValue("m_hController")
		if !hasProp {
			return nil
		}

		player := p.GameState().Participants().FindByHandle64(controllerProp.Handle())
		if player == nil {
			return nil
		}
		if player.SteamID64 == 0 {
			return nil
		}
		return getMoveDataFromPlayer(player)
	}
	p.RegisterEventHandler(func(events.FrameDone) {
		for _, pl := range p.GameState().Participants().All() {
			mv := mapPlayerEx[pl.SteamID64]
			if mv == nil {
				//fmt.Printf("Player %s is not valid\n", pl.Name)
				continue
			}
			if !mv.Tracking {
				continue
			}
			pawnEntity := mv.pl.PlayerPawnEntity()
			if pawnEntity == nil || pawnEntity.ServerClass().Name() != "CCSPlayerPawn" {
				continue
			}
			prop, _ := pawnEntity.PropertyValue("m_hGroundEntity")
			groundEnt := p.GameState().EntityByHandle(prop.Handle())
			if groundEnt == nil {
				mv.AirTime++
				if mv.GetTurning() > 0 {
					diff := mv.OldYaw - mv.pl.ViewDirectionY()
					if diff < 0 {
						diff += 360
					}
					mv.AirTurnData = append(mv.AirTurnData, float64(diff))
				} else if mv.GetTurning() < 0 {
					diff := mv.pl.ViewDirectionY() - mv.OldYaw
					if diff < 0 {
						diff += 360
					}
					mv.AirTurnData = append(mv.AirTurnData, float64(diff))
				}

				if mv.TurnState+mv.GetTurning() == 0 && mv.TurnState != 0 {
					mv.GoodTurns++
				}
			}
			mv.TurnState = mv.GetTurning()
			mv.OldYaw = mv.pl.ViewDirectionY()
		}
	})
	p.RegisterEventHandler(func(events.DataTablesParsed) {
		p.ServerClasses().FindByName("CCSPlayerPawn").OnEntityCreated(func(pawnEntity st.Entity) {
			buttonProp := pawnEntity.Property("m_pMovementServices.m_nButtonDownMaskPrev")
			if buttonProp != nil {
				buttonChanged := func(val st.PropertyValue) {
					// Can dead players press buttons? What about freeze time?
					// Let's just ignore these questions for now.
					mv := getOverlapDataFromPawnEntity(pawnEntity)

					if mv == nil || !mv.Tracking {
						return
					}
					// Pressing any key?
					if val.UInt64()&0x618 != 0 && mv.OldButtons == 0 {
						mv.LastMoveAttemptTick = p.GameState().IngameTick()
						//fmt.Printf("Player %s started moving @%d\n", ol.pl.Name, ol.LastMoveAttemptTick)
					} else if val.UInt64()&0x618 == 0 && mv.OldButtons != 0 {
						mv.NumMoveTicks += p.GameState().IngameTick() - mv.LastMoveAttemptTick
						//fmt.Printf("Player %s stopped moving @%d (+%d)\n", ol.pl.Name, p.GameState().IngameTick(), p.GameState().IngameTick()-ol.LastMoveAttemptTick)
					}

					// Overlapping?
					if (val.UInt64()&0x10 != 0) && (val.UInt64()&0x8 != 0) {
						mv.IsWSOverlapping = true
						mv.LastWSOverlapTick = p.GameState().IngameTick()
						// fmt.Printf("%s W/S overlapped at tick %d\n",
						// 	ol.pl.Name,
						// 	p.GameState().IngameTick())
					} else if mv.IsWSOverlapping {
						mv.IsWSOverlapping = false
						numOverlapTick := p.GameState().IngameTick() - mv.LastWSOverlapTick
						if numOverlapTick > 1 {
							mv.WSOverlapTicks = append(mv.WSOverlapTicks, numOverlapTick)
						} else {
							mv.GoodGroundSwitchCount++
						}
					}

					if (val.UInt64()&0x200 != 0) && (val.UInt64()&0x400 != 0) {
						mv.IsADOverlapping = true
						mv.LastADOverlapTick = p.GameState().IngameTick()
						// fmt.Printf("%s A/D overlapped at tick %d\n",
						// 	ol.pl.Name,
						// 	p.GameState().IngameTick())
					} else if mv.IsADOverlapping {
						mv.IsADOverlapping = false
						numOverlapTick := p.GameState().IngameTick() - mv.LastADOverlapTick
						if numOverlapTick > 1 {
							mv.ADOverlapTicks = append(mv.ADOverlapTicks, numOverlapTick)
						} else {
							mv.GoodGroundSwitchCount++
						}
					}
					if mv.checkGoodSwitch(val.UInt64()) {
						mv.GoodGroundSwitchCount++
					}
					// Doesn't really need other buttons.
					mv.OldButtons = val.UInt64() & 0x618
				}
				buttonProp.OnUpdate(buttonChanged)
			}
		})
	})
	spewReport := func() {
		if reported {
			return
		}
		if verbose {
			fmt.Printf("Game duration: %d ticks (%f minutes)\n", p.GameState().IngameTick(), float64(p.GameState().IngameTick())/64.0/60.0)
		}
		output.WriteString("Date,SteamID64,Name,A/D overlap (instances),A/D overlap (ticks),A/D overlap (tick/instance),W/S overlap (instances),W/S overlap (ticks),W/S overlap (tick/instance),Good Strafe Switch,Total Move Ticks,Good Airstrafe Turns,Total Airtime,Subtick Data,Subtick Decode Errors,Null FB Detected,Null FB Streak,Null FB Required,Null FB Perfect,Null FB Overlaps,Null FB Underlap Count,Null FB Underlap Min (ms),Null FB Underlap Max (ms),Null FB Underlap Mean (ms),Null FB Underlap Median (ms),Null FB Underlap P90 (ms),Null FB Status,Null LR Detected,Null LR Streak,Null LR Required,Null LR Perfect,Null LR Overlaps,Null LR Underlap Count,Null LR Underlap Min (ms),Null LR Underlap Max (ms),Null LR Underlap Mean (ms),Null LR Underlap Median (ms),Null LR Underlap P90 (ms),Null LR Status\n")
		for _, pl := range p.GameState().Participants().All() {
			mv := mapPlayerEx[pl.SteamID64]
			if mv == nil {
				//fmt.Printf("Player %s is not valid\n", pl.Name)
				continue
			}
			mv.Tracking = false
			// Finalize the stats.
			if mv.OldButtons != 0 {
				mv.NumMoveTicks += p.GameState().IngameTick() - mv.LastMoveAttemptTick
			}
			if mv.IsWSOverlapping {
				mv.IsWSOverlapping = false
				numOverlapTick := p.GameState().IngameTick() - mv.LastWSOverlapTick
				if numOverlapTick > 1 {
					mv.WSOverlapTicks = append(mv.WSOverlapTicks, numOverlapTick)
				} else {
					mv.GoodGroundSwitchCount++
				}
			}
			if mv.IsADOverlapping {
				mv.IsADOverlapping = false
				numOverlapTick := p.GameState().IngameTick() - mv.LastADOverlapTick
				if numOverlapTick > 1 {
					mv.ADOverlapTicks = append(mv.ADOverlapTicks, numOverlapTick)
				} else {
					mv.GoodGroundSwitchCount++
				}
			}

			if verbose {
				fmt.Printf("%s (%d): W/S overlap ticks %d, A/D overlap ticks %d, good key switch count %d, total move ticks %d, good turns %d, airtime %d",
					mv.pl.Name, mv.pl.SteamID64, mv.GetWSTotalOverlap(), mv.GetADTotalOverlap(), mv.GoodGroundSwitchCount, mv.NumMoveTicks, mv.GoodTurns, mv.AirTime)
				fmt.Println("")
			}
			// Airstrafe speed stuff
			// mean, _ := stats.Mean(ol.AirTurnData)
			// fmt.Printf(", average turn speed %f (%d samples total)\n", mean, len(ol.AirTurnData))
			// for i := 10; i < 100; i += 20 {
			// 	percentile, _ := stats.Percentile(ol.AirTurnData, float64(i))
			// 	fmt.Printf("%d%%: %f\n", i, percentile)
			// }

			WSavg := float32(0)
			if len(mv.WSOverlapTicks) > 0 {
				WSavg = float32(mv.GetWSTotalOverlap()) / float32(len(mv.WSOverlapTicks))
			}

			ADavg := float32(0)
			if len(mv.ADOverlapTicks) > 0 {
				ADavg = float32(mv.GetADTotalOverlap()) / float32(len(mv.ADOverlapTicks))
			}

			fbNullMetrics := AnalyzeNullsForAxis(mv.RecentForwardBackwardEvents, IN_FORWARD, IN_BACK)
			lrNullMetrics := AnalyzeNullsForAxis(mv.RecentLeftRightEvents, IN_MOVELEFT, IN_MOVERIGHT)

			line := fmt.Sprintf("%s,%d,%s,%d,%d,%f,%d,%d,%f,%d,%d,%d,%d,%t,%d,%t,%.2f,%.2f,%.2f,%.2f,%d,%.2f,%.2f,%.2f,%.2f,%.2f,%s,%t,%.2f,%.2f,%.2f,%.2f,%d,%.2f,%.2f,%.2f,%.2f,%.2f,%s\n",
				stat.ModTime().Format(time.DateTime),
				mv.pl.SteamID64,
				mv.pl.Name,
				len(mv.ADOverlapTicks),
				mv.GetADTotalOverlap(),
				ADavg,
				len(mv.WSOverlapTicks),
				mv.GetWSTotalOverlap(),
				WSavg,
				mv.GoodGroundSwitchCount,
				mv.NumMoveTicks,
				mv.GoodTurns,
				mv.AirTime,
				mv.HasSubtickData,
				mv.SubtickDecodeErrors,
				fbNullMetrics.Detected,
				fbNullMetrics.ConsecutivePerfect,
				fbNullMetrics.RequiredPerfect,
				fbNullMetrics.Perfect,
				fbNullMetrics.Overlaps,
				fbNullMetrics.UnderlapCount,
				fbNullMetrics.UnderlapMinMs,
				fbNullMetrics.UnderlapMaxMs,
				fbNullMetrics.UnderlapMeanMs,
				fbNullMetrics.UnderlapMedianMs,
				fbNullMetrics.UnderlapP90Ms,
				fbNullMetrics.Status,
				lrNullMetrics.Detected,
				lrNullMetrics.ConsecutivePerfect,
				lrNullMetrics.RequiredPerfect,
				lrNullMetrics.Perfect,
				lrNullMetrics.Overlaps,
				lrNullMetrics.UnderlapCount,
				lrNullMetrics.UnderlapMinMs,
				lrNullMetrics.UnderlapMaxMs,
				lrNullMetrics.UnderlapMeanMs,
				lrNullMetrics.UnderlapMedianMs,
				lrNullMetrics.UnderlapP90Ms,
				lrNullMetrics.Status)
			output.WriteString(line)

			if shouldDumpPlayer(debugPlayer, mv.pl) {
				fbDebugMetrics, fbUnderlaps := AnalyzeNullsForAxisWithDetails(mv.RecentForwardBackwardEvents, IN_FORWARD, IN_BACK)
				lrDebugMetrics, lrUnderlaps := AnalyzeNullsForAxisWithDetails(mv.RecentLeftRightEvents, IN_MOVELEFT, IN_MOVERIGHT)

				fmt.Printf("Subtick underlap stats for %s (%d):\n", mv.pl.Name, mv.pl.SteamID64)
				dumpUnderlapStats("FB", fbDebugMetrics, fbUnderlaps)
				dumpUnderlapStats("LR", lrDebugMetrics, lrUnderlaps)

				allEvents := make([]InputEvent, 0, len(mv.RecentForwardBackwardEvents)+len(mv.RecentLeftRightEvents))
				allEvents = append(allEvents, mv.RecentForwardBackwardEvents...)
				allEvents = append(allEvents, mv.RecentLeftRightEvents...)
				sort.Slice(allEvents, func(i, j int) bool {
					if allEvents[i].CmdNum != allEvents[j].CmdNum {
						return allEvents[i].CmdNum < allEvents[j].CmdNum
					}
					if allEvents[i].Fraction != allEvents[j].Fraction {
						return allEvents[i].Fraction < allEvents[j].Fraction
					}
					if allEvents[i].Button != allEvents[j].Button {
						return allEvents[i].Button < allEvents[j].Button
					}
					if allEvents[i].Pressed != allEvents[j].Pressed {
						return allEvents[i].Pressed && !allEvents[j].Pressed
					}
					return allEvents[i].Analog && !allEvents[j].Analog
				})

				fmt.Printf("Subtick all movement events for %s (%d): %d events\n", mv.pl.Name, mv.pl.SteamID64, len(allEvents))
				for _, ev := range allEvents {
					fmt.Printf("  cmd=%d frac=%.6f button=%s pressed=%t analog=%t\n", ev.CmdNum, ev.Fraction, buttonName(ev.Button), ev.Pressed, ev.Analog)
				}
			}
		}
		reported = true
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

func buttonName(button uint64) string {
	switch button {
	case IN_FORWARD:
		return "IN_FORWARD"
	case IN_BACK:
		return "IN_BACK"
	case IN_MOVELEFT:
		return "IN_MOVELEFT"
	case IN_MOVERIGHT:
		return "IN_MOVERIGHT"
	default:
		return fmt.Sprintf("0x%x", button)
	}
}

func dumpUnderlapStats(axis string, metrics NullAxisMetrics, underlaps []float64) {
	if len(underlaps) == 0 {
		fmt.Printf("  %s underlaps: none (status=%s, samples=%d)\n", axis, metrics.Status, metrics.Samples)
		return
	}

	underlapMs := make([]float64, len(underlaps))
	for i, d := range underlaps {
		underlapMs[i] = d * 1000.0
	}
	sort.Float64s(underlapMs)

	sum := 0.0
	for _, v := range underlapMs {
		sum += v
	}
	mean := sum / float64(len(underlapMs))
	min := underlapMs[0]
	max := underlapMs[len(underlapMs)-1]
	median := underlapMs[len(underlapMs)/2]
	p90 := underlapMs[int(float64(len(underlapMs)-1)*0.9)]

	fmt.Printf("  %s underlaps: count=%d min=%.3fms max=%.3fms mean=%.3fms median=%.3fms p90=%.3fms\n", axis, len(underlapMs), min, max, mean, median, p90)
	fmt.Printf("  %s underlap times (ms):\n", axis)
	for _, v := range underlapMs {
		fmt.Printf("    %.6f\n", v)
	}
}
