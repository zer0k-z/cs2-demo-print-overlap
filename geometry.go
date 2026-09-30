package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// World collision of a map, used to find the floor under a player. The collision is dumped from the map's
// VPK by tools/physdump (C#, ValveResourceFormat from NuGet) the first time a map is needed, and cached in
// the user cache directory so nothing is written to the repository.

const (
	walkableNormalZ = 0.7  // sv_standable_normal
	playerHullHalf  = 16.0 // The player hull is 32x32 units wide
	geometryCell    = 256.0
)

type triangle struct {
	v          [3][3]float64
	minX, maxX float64
	minY, maxY float64
}

type Geometry struct {
	triangles []triangle
	grid      map[[2]int][]int32
}

// Highest walkable surface under a player hull footprint at (x, y) that is at or below maxZ.
func (g *Geometry) FloorBelow(x, y, maxZ float64) (float64, bool) {
	x0, x1, y0, y1 := x-playerHullHalf, x+playerHullHalf, y-playerHullHalf, y+playerHullHalf
	best, found := -math.MaxFloat64, false
	seen := map[int32]bool{}
	for cx := int(math.Floor(x0 / geometryCell)); cx <= int(math.Floor(x1/geometryCell)); cx++ {
		for cy := int(math.Floor(y0 / geometryCell)); cy <= int(math.Floor(y1/geometryCell)); cy++ {
			for _, i := range g.grid[[2]int{cx, cy}] {
				if seen[i] {
					continue
				}
				seen[i] = true
				t := &g.triangles[i]
				if t.maxX < x0 || t.minX > x1 || t.maxY < y0 || t.minY > y1 {
					continue
				}
				// A hull swept down onto a plane first touches it where the plane is highest over the
				// footprint, which is a vertex of the triangle clipped to the footprint.
				poly := [][3]float64{t.v[0], t.v[1], t.v[2]}
				poly = clipPolygon(poly, 0, x0, false)
				poly = clipPolygon(poly, 0, x1, true)
				poly = clipPolygon(poly, 1, y0, false)
				poly = clipPolygon(poly, 1, y1, true)
				for _, p := range poly {
					if p[2] <= maxZ && p[2] > best {
						best, found = p[2], true
					}
				}
			}
		}
	}
	return best, found
}

func clipPolygon(poly [][3]float64, axis int, bound float64, keepBelow bool) [][3]float64 {
	inside := func(p [3]float64) bool {
		if keepBelow {
			return p[axis] <= bound
		}
		return p[axis] >= bound
	}
	var out [][3]float64
	for i := range poly {
		cur, next := poly[i], poly[(i+1)%len(poly)]
		if inside(cur) {
			out = append(out, cur)
		}
		if inside(cur) != inside(next) {
			t := (bound - cur[axis]) / (next[axis] - cur[axis])
			var p [3]float64
			for k := range p {
				p[k] = cur[k] + t*(next[k]-cur[k])
			}
			out = append(out, p)
		}
	}
	return out
}

// Loads a physdump file, keeping the walkable triangles a player collides with.
func loadGeometry(path string) (*Geometry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var attributeCount int32
	if err := binary.Read(r, binary.LittleEndian, &attributeCount); err != nil {
		return nil, err
	}
	solid := make([]bool, attributeCount)
	for i := range solid {
		s, err := readDotnetString(r)
		if err != nil {
			return nil, err
		}
		tags := strings.Split(strings.Split(s, "|")[0], ",")
		solid[i] = true
		for _, tag := range tags {
			// Surfaces that don't block players.
			if tag == "csgo_grenadeclip" || tag == "sky" || (tag == "blocksound" && len(tags) == 1) {
				solid[i] = false
			}
		}
	}
	var count int32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	g := &Geometry{grid: map[[2]int][]int32{}}
	for i := int32(0); i < count; i++ {
		var attribute int32
		var v [9]float32
		if err := binary.Read(r, binary.LittleEndian, &attribute); err != nil {
			return nil, err
		}
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		if attribute < 0 || int(attribute) >= len(solid) || !solid[attribute] {
			continue
		}
		var t triangle
		for k := 0; k < 3; k++ {
			t.v[k] = [3]float64{float64(v[k*3]), float64(v[k*3+1]), float64(v[k*3+2])}
		}
		a, b, c := t.v[0], t.v[1], t.v[2]
		ux, uy, uz := b[0]-a[0], b[1]-a[1], b[2]-a[2]
		vx, vy, vz := c[0]-a[0], c[1]-a[1], c[2]-a[2]
		nx, ny, nz := uy*vz-uz*vy, uz*vx-ux*vz, ux*vy-uy*vx
		length := math.Sqrt(nx*nx + ny*ny + nz*nz)
		// Hull winding isn't consistent, so accept either side facing up.
		if length == 0 || math.Abs(nz)/length < walkableNormalZ {
			continue
		}
		t.minX, t.maxX = math.Min(a[0], math.Min(b[0], c[0])), math.Max(a[0], math.Max(b[0], c[0]))
		t.minY, t.maxY = math.Min(a[1], math.Min(b[1], c[1])), math.Max(a[1], math.Max(b[1], c[1]))
		index := int32(len(g.triangles))
		g.triangles = append(g.triangles, t)
		for cx := int(math.Floor(t.minX / geometryCell)); cx <= int(math.Floor(t.maxX/geometryCell)); cx++ {
			for cy := int(math.Floor(t.minY / geometryCell)); cy <= int(math.Floor(t.maxY/geometryCell)); cy++ {
				g.grid[[2]int{cx, cy}] = append(g.grid[[2]int{cx, cy}], index)
			}
		}
	}
	return g, nil
}

// .NET BinaryWriter string: 7-bit encoded length followed by UTF-8 bytes.
func readDotnetString(r *bufio.Reader) (string, error) {
	length, shift := 0, 0
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		length |= int(b&0x7f) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
	}
	buf := make([]byte, length)
	_, err := io.ReadFull(r, buf)
	return string(buf), err
}

type geometryEntry struct {
	once sync.Once
	g    *Geometry
	err  error
}

type GeometryProvider struct {
	csgoDir     string // game/csgo of a CS2 install, found automatically when empty
	physdumpDir string // tools/physdump
	mu          sync.Mutex
	maps        map[string]*geometryEntry
	buildOnce   sync.Once
	buildDLL    string
	buildErr    error
}

func NewGeometryProvider(csgoDir string) *GeometryProvider {
	return &GeometryProvider{csgoDir: csgoDir, physdumpDir: defaultPhysdumpDir(), maps: map[string]*geometryEntry{}}
}

// Returns the collision of a map, dumping it first if it isn't cached yet. Safe to call from several demos
// at once, each map is only dumped once.
func (gp *GeometryProvider) Get(mapName string, demoPath string) (*Geometry, error) {
	gp.mu.Lock()
	entry := gp.maps[mapName]
	if entry == nil {
		entry = &geometryEntry{}
		gp.maps[mapName] = entry
	}
	gp.mu.Unlock()
	entry.once.Do(func() {
		entry.g, entry.err = gp.load(mapName, demoPath)
	})
	return entry.g, entry.err
}

func (gp *GeometryProvider) load(mapName string, demoPath string) (*Geometry, error) {
	if !regexp.MustCompile(`^[\w\-]+$`).MatchString(mapName) {
		return nil, fmt.Errorf("unexpected map name %q", mapName)
	}
	vpk, err := gp.findMapVPK(mapName, demoPath)
	if err != nil {
		return nil, err
	}
	stat, err := os.Stat(vpk)
	if err != nil {
		return nil, err
	}
	cacheDir, err := cacheDirectory()
	if err != nil {
		return nil, err
	}
	// Keyed on the VPK's size and modification time, so a map update gets dumped again.
	dump := filepath.Join(cacheDir, "maps", fmt.Sprintf("%s-%d-%d.bin", mapName, stat.Size(), stat.ModTime().Unix()))
	if _, err := os.Stat(dump); err != nil {
		dll, err := gp.buildPhysdump(cacheDir)
		if err != nil {
			return nil, err
		}
		fmt.Printf("Dumping %s collision from %s\n", mapName, vpk)
		cmd := exec.Command("dotnet", dll, vpk, "maps/"+mapName+"/world_physics.vmdl_c", dump)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("physdump failed: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	return loadGeometry(dump)
}

// Builds tools/physdump into the cache directory. NuGet downloads ValveResourceFormat on the first build.
func (gp *GeometryProvider) buildPhysdump(cacheDir string) (string, error) {
	gp.buildOnce.Do(func() {
		if _, err := exec.LookPath("dotnet"); err != nil {
			gp.buildErr = fmt.Errorf("dotnet SDK not found, it is needed to read map collision: %v", err)
			return
		}
		artifacts := filepath.Join(cacheDir, "physdump-build")
		fmt.Println("Building tools/physdump (first run downloads ValveResourceFormat from NuGet)")
		cmd := exec.Command("dotnet", "build", filepath.Join(gp.physdumpDir, "physdump.csproj"), "-c", "Release", "--artifacts-path", artifacts, "-v", "quiet", "-nologo")
		if out, err := cmd.CombinedOutput(); err != nil {
			gp.buildErr = fmt.Errorf("building physdump failed: %v: %s", err, strings.TrimSpace(string(out)))
			return
		}
		gp.buildDLL = filepath.Join(artifacts, "bin", "physdump", "release", "physdump.dll")
		if _, err := os.Stat(gp.buildDLL); err != nil {
			gp.buildErr = fmt.Errorf("physdump build output not found: %v", err)
		}
	})
	return gp.buildDLL, gp.buildErr
}

func cacheDirectory() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "cs2-print-overlap"), nil
}

// tools/physdump next to this source file, which also works with go run.
func defaultPhysdumpDir() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		dir := filepath.Join(filepath.Dir(file), "tools", "physdump")
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
	}
	return filepath.Join("tools", "physdump")
}

// Looks for maps/<map>.vpk in, in order: -csgo, the CS2 install the demo sits in (e.g. csgo/replays),
// and the Steam library folders.
func (gp *GeometryProvider) findMapVPK(mapName string, demoPath string) (string, error) {
	var candidates []string
	if gp.csgoDir != "" {
		candidates = append(candidates, gp.csgoDir)
	}
	if abs, err := filepath.Abs(demoPath); err == nil {
		slashed := filepath.ToSlash(abs)
		if i := strings.Index(strings.ToLower(slashed), "/game/csgo/"); i >= 0 {
			candidates = append(candidates, filepath.FromSlash(slashed[:i+len("/game/csgo")]))
		}
	}
	for _, library := range steamLibraries() {
		candidates = append(candidates, filepath.Join(library, "steamapps", "common", "Counter-Strike Global Offensive", "game", "csgo"))
	}
	for _, dir := range candidates {
		vpk := filepath.Join(dir, "maps", mapName+".vpk")
		if _, err := os.Stat(vpk); err == nil {
			return vpk, nil
		}
	}
	return "", fmt.Errorf("maps/%s.vpk not found (pass -csgo <path to game/csgo>)", mapName)
}

func steamLibraries() []string {
	var roots []string
	if runtime.GOOS == "windows" {
		for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles"} {
			if dir := os.Getenv(env); dir != "" {
				roots = append(roots, filepath.Join(dir, "Steam"))
			}
		}
	} else if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, ".steam", "steam"), filepath.Join(home, ".local", "share", "Steam"))
	}
	pathRe := regexp.MustCompile(`"path"\s+"([^"]+)"`)
	var libraries []string
	for _, root := range roots {
		libraries = append(libraries, root)
		data, err := os.ReadFile(filepath.Join(root, "steamapps", "libraryfolders.vdf"))
		if err != nil {
			continue
		}
		for _, m := range pathRe.FindAllStringSubmatch(string(data), -1) {
			libraries = append(libraries, strings.ReplaceAll(m[1], `\\`, `\`))
		}
	}
	return libraries
}
