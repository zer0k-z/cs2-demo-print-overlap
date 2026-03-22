Prints movement overlap statistics and subtick-based null detection metrics from CS2 demos.

Null detection uses decoded `svc_UserCmds` payloads and subtick move steps. If subtick usercmd data is missing or undecodable in a demo, null-related columns are still emitted with status fields explaining why detection could not run.

Usage: `go run print-overlap.go -demo /path/to/demo.dem`

Optional arguments:

- `-dir <directory>` for directory parsing
- `-v` for verbose output to console
- `max-concurrent <#>` to limit the number of concurrent parsing (default 8)

CSV output now includes additional columns for:

- whether subtick data was observed for a player
- subtick decode error count
- forward/backward null metrics (detected, streak, threshold, perfect count, overlap count, underlap statistics, status)
- left/right null metrics (detected, streak, threshold, perfect count, overlap count, underlap statistics, status)