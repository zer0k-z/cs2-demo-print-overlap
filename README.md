Looks for movement cheats in CS2 demos. It runs a subset of the [cs2kz-metamod](https://github.com/KZGlobalTeam/cs2kz-metamod) anticheat detections, plus a few checks that only make sense with a whole demo to look at.

Usage: `go run . -demo /path/to/demo.dem`

Optional arguments:

- `-dir <directory>` for directory parsing
- `-v` for verbose output to console (per player summary and every detection)
- `-debug-player <name or SteamID64>` to dump landing events and suspicious commands of a player
- `-csgo <path to game/csgo>` to read map collision from a specific CS2 install. By default it is found from the demo's location (e.g. `game/csgo/replays`) or the Steam library folders
- `-no-map` to skip the checks that need map collision
- `-max-concurrent <#>` to limit the number of concurrent parsing (default 8)

Requires Go 1.27 (demoinfocs-golang v6). The press height check also needs the .NET 10 SDK, see [Map collision](#map-collision).

## Checks

Ported from cs2kz-metamod:

- **bhop_hack**: too many consecutive perfect bhops, or a highly repetitive jump input pattern during a long perf chain
- **hyperscroll**: a very high number of jump inputs around each landing combined with a high perf ratio
- **invalid_input**: jump/duck presses or releases that are not accounted for in the command's subtick moves
- **subtick_spam**: commands carrying an excessive amount of subtick moves with view angle changes
- **desubtick**: the vast majority of subtick inputs landing exactly at the start of the tick, or subtick move lists the engine would discard

Specific to demos:

- **press_height**: where perfect bhop presses happen in the last 2 units above the floor. A press there grounds the player and is always perfect, and a human pressing somewhere in that range lands anywhere in it. Flagged when at least 60% of 20 or more subtick timed perfect bhops happen in the first fifth of it, right as the player gets within 2 units (about 20% is expected)
- **desubticked_jumps**: jumps without a subtick move are processed at the start of the tick, so a client that doesn't send them always jumps exactly there. Flagged when at least 80% of 20 or more normal (non-bhop) jumps do
- **input_consistency**: a real client derives the button states and the subtick moves from the same key events, so they always agree. Flagged when 10 or more commands have a jump press in the subtick moves that the button state doesn't show, a jump button change without any jump move, or a button state that doesn't continue from the previous command

## Output

A `<demo>_report` folder is written next to each demo:

- `summary.csv`: one row per player with their estimated fps and a verdict for every check (`FLAGGED`, `clean`, or why the check couldn't run)
- `detections.csv`: every detection of the cs2kz checks, with its tick and details
- `bhop.csv`: jumps around landings, split the way CS2 handles them: `perfect` (jump time equals the landing time), `in_window` (within 0.25 tick after the landing, speed kept), `deferred` (exactly one tick after the landing, speed lost) and jumpbugs/ledgegrabs, by jump input (scroll or key). Also the peak values the cs2kz bhop checks saw
- `press_height.csv`: height above the floor and time since getting within 2 units at each perfect bhop press
- `subtick.csv`: the counters behind the cs2kz subtick checks, and the share of normal jumps at the start of a tick
- `inputs.csv`: the jump input consistency counters

Detections keep running after a player is flagged, and the buffers of the detection that triggered are reset, so the count is the number of independent detections.

## How demos are read

- Most user commands are stored in `CMsgServerUserCmd.delta_data`, encoded against the previous command. demoinfocs-golang v6 reconstructs them (`UserCmdParsingFull`). Commands the server ignored (`execution_notes`, e.g. `cannot_move` during freeze time or `dead`) and resent commands are skipped.
- Landing and jump times come from the pawn's `m_nLastLandedTick/Frac` and `m_nLastJumpTick/Frac`. These ticks count 2 per game tick, while the fractions are fractions of a game tick. Landings the pawn doesn't record (on the way up onto a ledge, crouchbugs) are taken from the ground flag, or from a jump straight out of the air.
- The cs2kz bhop checks count perfect and in-window jumps as perfs, like the server's `IsPerfing(true)`.
- Jump inputs for the cs2kz Hyperscroll/pattern checks come from the commands' subtick moves. When a demo has no user commands for a player, they come from the pawn's `m_nLastActualJumpPressTick` instead, which only exposes one press per tick.
- `m_vecLastMovementImpulses` isn't networked, so it is emulated from the previous commands for the Desubticking check.
- The fps is the most common gap between the frames in the commands' input history. The client sends every frame of a command when there are at most 4, and 4 spread over the command otherwise (`cl_csgo_shoot_trim_input_frames`), so only commands with 2 or 3 frames are used.
- `sv_autobunnyhopping` is assumed off, and `sv_jump_spam_penalty_time`, `sv_bhop_time_window` and `sv_gravity` at their default values.

## Map collision

The press height check needs the floor under the player. The first time a map comes up, `tools/physdump` (C#, [ValveResourceFormat](https://github.com/ValveResourceFormat/ValveResourceFormat) from NuGet) is built and dumps the map's world collision from `maps/<map>.vpk`. The build and the dumps go to the user cache directory (`%LocalAppData%\cs2-print-overlap` on Windows), nothing is written to the repository. A map is dumped again when its VPK changes.

Only world collision is used: players, props and moving entities outside it give wrong heights, which end up as outliers instead of measured presses. Workshop maps aren't supported.
