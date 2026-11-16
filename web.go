package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed index.html
var indexHTML []byte

//go:embed viz.js
var vizJS []byte

// App ties the pieces together for the control page.
type App struct {
	show        atomic.Pointer[Show] // the lights being driven; swapped when the selected areas change
	hue         *Hue
	reconfig    chan struct{} // poked when the selected areas change
	store       *Store
	dj          *DJLink
	tracker     *Tracker
	link        *VCDJ
	linkAllowed bool
	started     time.Time
	streaming   atomic.Bool
	lastErr     atomic.Value // string: why the light stream last stopped

	pmu     sync.Mutex
	paused  bool          // the lights are handed back until the page starts the sync again
	pchange chan struct{} // closed, and replaced, each time paused changes
}

// pauseState reports whether the sync is paused, with a channel that is
// closed the next time that changes.
func (a *App) pauseState() (bool, <-chan struct{}) {
	a.pmu.Lock()
	defer a.pmu.Unlock()
	if a.pchange == nil {
		a.pchange = make(chan struct{})
	}
	return a.paused, a.pchange
}

func (a *App) isPaused() bool {
	p, _ := a.pauseState()
	return p
}

// setPaused pauses or restarts the sync and remembers the choice, so a
// paused copy is still paused after a restart.
func (a *App) setPaused(p bool) error {
	a.pmu.Lock()
	if a.paused == p {
		a.pmu.Unlock()
		return nil
	}
	a.paused = p
	if a.pchange != nil {
		close(a.pchange)
	}
	a.pchange = make(chan struct{})
	a.pmu.Unlock()
	if a.store == nil {
		return nil
	}
	return a.store.Update(func(c *Config) { c.Paused = p })
}

func (a *App) errText() string {
	s, _ := a.lastErr.Load().(string)
	return s
}

type statusJSON struct {
	Locked    bool         `json:"locked"`
	Player    int          `json:"player"`
	BPM       float64      `json:"bpm"`
	BeatInBar int          `json:"beat_in_bar"`
	Streaming bool         `json:"streaming"`
	Paused    bool         `json:"paused"`
	GearLost  bool         `json:"gear_lost"` // no deck or mixer heard, and we have been up long enough to expect one
	Error     string       `json:"error"`
	Area      string       `json:"area"`
	Channels  int          `json:"channels"`
	Devices   []Device     `json:"devices"`
	Decks     []DeckStatus `json:"decks"`
	Lights    []lightJSON  `json:"lights"`
	Link      linkJSON     `json:"link"`
	Phrase    phraseJSON   `json:"phrase"`
	Viz       vizJSON      `json:"viz"`
}

// vizJSON lets the page run its own smooth beat clock. Pos is the beat
// position as heard in the room: the lights' head start is taken back out,
// because a screen needs no lead.
type vizJSON struct {
	Pos      float64  `json:"pos"`       // beats; whole multiples of 4 are downbeats
	PeriodMs float64  `json:"period_ms"` // one beat
	Drop     bool     `json:"drop"`      // this beat is a chorus landing
	FillAt   float64  `json:"fill_at"`   // beat position of the next build-up hit; 0 if build-ups are off
	BlockAt  float64  `json:"block_at"`  // beat position of the next scene change; 0 if scenes only change by hand
	Offbeat  float64  `json:"offbeat"`   // strength of beats 2-4 relative to beat 1
	Colors   []string `json:"colors"`    // the room's colours right now, left to right
}

func hexColour(c RGB) string {
	b := func(v float64) int { return int(clampF(v, 0, 1)*255 + 0.5) }
	return fmt.Sprintf("#%02x%02x%02x", b(c.R), b(c.G), b(c.B))
}

func (a *App) viz(now time.Time, ph Phase, s Settings) vizJSON {
	v := vizJSON{Offbeat: s.OffbeatLevel, Drop: s.DropFlash > 0 && isDrop(ph)}
	for _, c := range a.show.Load().engine.Theme(now, ph, s, 5) {
		v.Colors = append(v.Colors, hexColour(c))
	}
	if ph.Locked {
		v.PeriodMs = ph.Period * 1000
		here := float64(ph.BeatIdx) + ph.Frac
		v.Pos = here - s.LeadMs/v.PeriodMs
		if _, _, left := fillState(ph, s); left > 0 {
			v.FillAt = here + left
		}
		if s.VizBars > 0 {
			_, into := blocks(ph, 4*int64(s.VizBars))
			v.BlockAt = here + float64(4*s.VizBars) - into
		}
	}
	return v
}

// linkJSON describes the optional deck link.
type linkJSON struct {
	Allowed bool   `json:"allowed"` // false unless started with -experimental-deck-link
	Active  bool   `json:"active"`
	Number  int    `json:"number"`
	Note    string `json:"note"`
}

// phraseJSON is the section of the track now playing, when known.
type phraseJSON struct {
	Known     bool    `json:"known"`
	Label     string  `json:"label"`
	Energy    float64 `json:"energy"`
	Beat      int64   `json:"beat"`       // beats into the phrase
	TrackBeat int64   `json:"track_beat"` // beat number within the track, 0 if unknown
	Phrases   int     `json:"phrases"`    // how many the track has
}

// lightJSON is one channel on the room map: its position and what it is
// showing right now.
type lightJSON struct {
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Z     float64 `json:"z"`
	Color string  `json:"color"`
	Slow  bool    `json:"slow,omitempty"` // a follower: ordinary commands, not the stream
}

func (a *App) lights(now time.Time, ph Phase, s Settings) []lightJSON {
	sh := a.show.Load()
	cols := sh.engine.Render(now, ph, s)
	lit := map[int]bool{} // the followers whose turn this beat is
	for _, t := range sh.followerTurn(ph, s) {
		lit[sh.followers[t].idx] = true
	}
	out := make([]lightJSON, len(cols))
	b := func(v float64) int { return int(math.Sqrt(clampF(v, 0, 1))*255 + 0.5) } // brighten for screens
	for i, c := range sh.engine.chans {
		col := cols[i]
		if c.Slow && ph.Locked && !lit[i] {
			// a follower between its flashes rests at the floor
			if m := math.Max(col.R, math.Max(col.G, col.B)); m > 0 {
				col = col.scale(math.Min(1, s.Floor/m))
			}
		}
		out[i] = lightJSON{X: c.X, Y: c.Y, Z: c.Z, Slow: c.Slow, Color: fmt.Sprintf("#%02x%02x%02x", b(col.R), b(col.G), b(col.B))}
	}
	return out
}

// areaJSONOut is one Entertainment area as the control page lists it.
type areaJSONOut struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Lights   int    `json:"lights"`
	Channels int    `json:"channels"`
	Selected bool   `json:"selected"`
	Role     string `json:"role"`  // streamed | follows | ""
	Extra    int    `json:"extra"` // lights this area adds beyond the streamed one
}

func (a *App) areaList() ([]areaJSONOut, string, error) {
	areas, err := a.hue.Areas()
	if err != nil {
		return nil, "", err
	}
	sh := a.show.Load()
	sel := map[string]bool{}
	for _, id := range a.store.Config().SelectedAreas() {
		sel[id] = true
	}
	streamed := map[string]bool{}
	for _, c := range sh.streamCh {
		streamed[c.LightID] = true
	}
	out := []areaJSONOut{}
	for _, ar := range areas {
		o := areaJSONOut{ID: ar.ID, Name: ar.Name, Kind: ar.Type, Lights: len(ar.Lights), Channels: len(ar.Channels), Selected: sel[ar.ID]}
		for _, l := range ar.Lights {
			if !streamed[l.ID] {
				o.Extra++
			}
		}
		switch {
		case ar.ID == sh.stream.ID:
			o.Role = "streamed"
		case sel[ar.ID]:
			o.Role = "follows"
		}
		out = append(out, o)
	}
	note := ""
	if n := len(sh.followers); n > 0 {
		per := sh.followersPerBeat(a.store.Settings())
		note = fmt.Sprintf("%d lights follow by ordinary commands, taking turns to flash: %d on each beat, so each flashes about every %d beats", n, per, (n+per-1)/per)
		if n == 1 {
			note = "1 light follows by ordinary commands and flashes on every beat"
		}
	}
	return out, note, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func (a *App) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("/viz.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(vizJS)
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		s := a.store.Settings()
		ph := a.tracker.Phase(now, s)
		devices := a.dj.Devices(now)
		link := linkJSON{Allowed: a.linkAllowed}
		if a.link != nil {
			link.Active, link.Number, link.Note = a.link.Info()
		}
		paused, _ := a.pauseState()
		phrase := phraseJSON{Known: ph.PhraseKnown, Label: ph.PhraseLabel, Energy: ph.Energy, Beat: ph.PhraseBeat, TrackBeat: ph.TrackBeat}
		if ph.Structure != nil {
			phrase.Phrases = len(ph.Structure.Phrases)
		}
		writeJSON(w, map[string]any{
			"presets":  a.store.PresetInfo(),
			"settings": s,
			"status": statusJSON{
				Locked: ph.Locked, Player: ph.Player, BPM: math.Round(ph.BPM*100) / 100, BeatInBar: ph.BeatInBar,
				Streaming: a.streaming.Load(), Paused: paused, Error: a.errText(), Area: strings.Join(a.show.Load().names, " + "), Channels: len(a.show.Load().engine.chans),
				GearLost: len(devices) == 0 && now.Sub(a.started) > 10*time.Second,
				Devices:  devices, Decks: a.tracker.Decks(now, s),
				Lights: a.lights(now, ph, s),
				Link:   link, Phrase: phrase, Viz: a.viz(now, ph, s),
			},
		})
	})
	mux.HandleFunc("/api/pause", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Paused bool `json:"paused"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := a.setPaused(req.Paused); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"paused": req.Paused})
	})
	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		s := a.store.Settings() // start from current so partial updates work
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&s); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s, err := a.store.SetSettings(s)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, s)
	})
	mux.HandleFunc("/api/areas", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var req struct {
				IDs []string `json:"ids"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			areas, err := a.hue.Areas()
			if err != nil {
				http.Error(w, "cannot reach the Hue bridge: "+err.Error(), http.StatusBadGateway)
				return
			}
			next, err := buildShow(areas, req.IDs)
			if err != nil {
				http.Error(w, "choose at least one area that has lights", http.StatusBadRequest)
				return
			}
			var keep []string
			for _, ar := range areas { // keep only ids the bridge has, in the bridge's order
				for _, id := range req.IDs {
					if id == ar.ID && len(ar.Channels) > 0 {
						keep = append(keep, id)
					}
				}
			}
			if err := a.store.Update(func(c *Config) { c.Areas, c.AreaID, c.AreaName = keep, next.stream.ID, next.stream.Name }); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			select { // tell the light loop; it sets itself up again within a few seconds
			case a.reconfig <- struct{}{}:
			default:
			}
			writeJSON(w, map[string]any{"ok": true, "summary": next.describe()})
			return
		}
		list, note, err := a.areaList()
		if err != nil {
			http.Error(w, "cannot reach the Hue bridge: "+err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{"areas": list, "note": note})
	})
	mux.HandleFunc("/api/presets", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct{ Action, Name, To string }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := a.store.PresetAction(req.Action, req.Name, req.To); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"presets": a.store.PresetInfo(), "settings": a.store.Settings()})
	})
	mux.HandleFunc("/api/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		s, _ := a.store.SetSettings(withLook(a.store.Settings(), defaultSettings())) // the look only; the light lead and deck options stay
		writeJSON(w, s)
	})
	return mux
}

// lanAddresses lists the addresses a phone could reach the control page on.
// The address this machine uses to reach the Hue bridge is on the home
// network by definition, so prefer it: listing every adapter would include
// VPN tunnels and virtual machines.
func lanAddresses(bridgeIP string) []string {
	if c, err := net.Dial("udp4", net.JoinHostPort(bridgeIP, "443")); err == nil {
		defer c.Close()
		if ua, ok := c.LocalAddr().(*net.UDPAddr); ok && !ua.IP.IsLoopback() {
			return []string{ua.IP.String()}
		}
	}
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ip.String())
	}
	return out
}
