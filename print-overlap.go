package main

import (
	"fmt"
	"os"

	ex "github.com/markus-wa/demoinfocs-golang/v4/examples"
	demoinfocs "github.com/markus-wa/demoinfocs-golang/v4/pkg/demoinfocs"
	common "github.com/markus-wa/demoinfocs-golang/v4/pkg/demoinfocs/common"
	events "github.com/markus-wa/demoinfocs-golang/v4/pkg/demoinfocs/events"
	st "github.com/markus-wa/demoinfocs-golang/v4/pkg/demoinfocs/sendtables"
)

type OverlapData struct {
	pl                  *common.Player
	LastWSOverlapTick   int
	LastADOverlapTick   int
	NumWSOverlapTick    int
	NumADOverlapTick    int
	IsWSOverlapping     bool
	IsADOverlapping     bool
	LastMoveAttemptTick int
	IsAttemptingToMove  bool
	NumMoveTicks        int
}

// Run like this: go run print-overlap.go -demo /path/to/demo.dem
func main() {
	mapPlayerEx := make(map[uint64]*OverlapData)

	f, err := os.Open(ex.DemoPathFromArgs())
	checkError(err)

	defer f.Close()

	p := demoinfocs.NewParser(f)
	defer p.Close()

	// Do not use this at the end of the game.
	getOverlapDataFromPawnEntity := func(pawnEntity st.Entity) *OverlapData {
		controllerProp, hasProp := pawnEntity.PropertyValue("m_hController")
		if !hasProp {
			return nil
		}

		player := p.GameState().Participants().FindByHandle64(controllerProp.Handle())

		if mapPlayerEx[player.SteamID64] == nil {
			mapPlayerEx[player.SteamID64] = &OverlapData{pl: player}
		}
		return mapPlayerEx[player.SteamID64]
	}
	p.RegisterEventHandler(func(events.DataTablesParsed) {
		p.ServerClasses().FindByName("CCSPlayerPawn").OnEntityCreated(func(pawnEntity st.Entity) {
			buttonProp := pawnEntity.Property("m_pMovementServices.m_nButtonDownMaskPrev")
			if buttonProp != nil {
				buttonChanged := func(val st.PropertyValue) {
					// Can dead players press buttons? What about freeze time?
					// Let's just ignore these questions for now.
					ol := getOverlapDataFromPawnEntity(pawnEntity)

					// Pressing any key?
					if val.S2UInt64()&0x618 != 0 && !ol.IsAttemptingToMove {
						ol.LastMoveAttemptTick = p.GameState().IngameTick()
						ol.IsAttemptingToMove = true
						//fmt.Printf("Player %s started moving @%d\n", ol.pl.Name, ol.LastMoveAttemptTick)
					} else if ol.IsAttemptingToMove {
						ol.NumMoveTicks += p.GameState().IngameTick() - ol.LastMoveAttemptTick
						ol.IsAttemptingToMove = false
						//fmt.Printf("Player %s stopped moving @%d (+%d)\n", ol.pl.Name, p.GameState().IngameTick(), p.GameState().IngameTick()-ol.LastMoveAttemptTick)
					}

					// Overlapping?
					if (val.S2UInt64()&0x10 != 0) && (val.S2UInt64()&0x8 != 0) {
						ol.IsWSOverlapping = true
						ol.LastWSOverlapTick = p.GameState().IngameTick()
						// fmt.Printf("%s W/S overlapped at tick %d\n",
						// 	ol.pl.Name,
						// 	p.GameState().IngameTick())
					} else if ol.IsWSOverlapping {
						ol.IsWSOverlapping = false
						ol.NumWSOverlapTick += p.GameState().IngameTick() - ol.LastWSOverlapTick
					}

					if (val.S2UInt64()&0x200 != 0) && (val.S2UInt64()&0x400 != 0) {
						ol.IsADOverlapping = true
						ol.LastADOverlapTick = p.GameState().IngameTick()
						// fmt.Printf("%s A/D overlapped at tick %d\n",
						// 	ol.pl.Name,
						// 	p.GameState().IngameTick())
					} else if ol.IsADOverlapping {
						ol.IsADOverlapping = false
						ol.NumADOverlapTick += p.GameState().IngameTick() - ol.LastADOverlapTick
					}
				}
				buttonProp.OnUpdate(buttonChanged)
			}
		})
	})

	p.RegisterEventHandler(func(events.AnnouncementWinPanelMatch) {
		fmt.Printf("Game duration: %d ticks (%f minutes)\n", p.GameState().IngameTick(), float64(p.GameState().IngameTick())/64.0)
		for _, pl := range p.GameState().Participants().All() {
			ol := mapPlayerEx[pl.SteamID64]
			if ol == nil {
				//fmt.Printf("Player %s is not valid\n", pl.Name)
				continue
			}

			// Finalize the stats.
			if ol.IsAttemptingToMove {
				ol.NumMoveTicks += p.GameState().IngameTick() - ol.LastMoveAttemptTick
				ol.IsAttemptingToMove = false
			}
			if ol.IsWSOverlapping {
				ol.IsWSOverlapping = false
				ol.NumWSOverlapTick += p.GameState().IngameTick() - ol.LastWSOverlapTick
			}
			if ol.IsADOverlapping {
				ol.IsADOverlapping = false
				ol.NumADOverlapTick += p.GameState().IngameTick() - ol.LastADOverlapTick
			}

			fmt.Printf("%s (%d): %d ticks overlap W/S, %d ticks overlap A/D, total move ticks %d\n",
				ol.pl.Name, ol.pl.SteamID64, ol.NumWSOverlapTick, ol.NumADOverlapTick, ol.NumMoveTicks)
		}
	})
	fmt.Printf("Parsing overlap data for %s\n", f.Name())
	fmt.Println("Keep in mind that this overlap data can be inaccurate and does not contain subtick information.")
	fmt.Println("----")
	// Parse to end
	err = p.ParseToEnd()

	checkError(err)
}

func checkError(err error) {
	if err != nil {
		panic(err)
	}
}
