package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

//go:embed index.html
var indexHTML []byte

//go:embed viz.js
var vizJS []byte

// App ties the pieces together for the control page.
type App struct {
	engine      *Engine
	store       *Store
	dj          *DJLink
	tracker     *Tracker
	area        Area
	link        *VCDJ
	linkAllowed bool
	started     time.Time
	streaming   atomic.Bool
	lastErr     atomic.Value // string: why the light stream last stopped
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
	for _, c := range a.engine.Theme(now, ph, s, 5) {
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
}

func (a *App) lights(now time.Time, ph Phase, s Settings) []lightJSON {
	cols := a.engine.Render(now, ph, s)
	out := make([]lightJSON, len(cols))
	b := func(v float64) int { return int(math.Sqrt(clampF(v, 0, 1))*255 + 0.5) } // brighten for screens
	for i, c := range a.engine.chans {
		out[i] = lightJSON{X: c.X, Y: c.Y, Z: c.Z, Color: fmt.Sprintf("#%02x%02x%02x", b(cols[i].R), b(cols[i].G), b(cols[i].B))}
	}
	return out
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
		phrase := phraseJSON{Known: ph.PhraseKnown, Label: ph.PhraseLabel, Energy: ph.Energy, Beat: ph.PhraseBeat, TrackBeat: ph.TrackBeat}
		if ph.Structure != nil {
			phrase.Phrases = len(ph.Structure.Phrases)
		}
		writeJSON(w, map[string]any{
			"presets":  a.store.PresetInfo(),
			"settings": s,
			"status": statusJSON{
				Locked: ph.Locked, Player: ph.Player, BPM: math.Round(ph.BPM*100) / 100, BeatInBar: ph.BeatInBar,
				Streaming: a.streaming.Load(), Error: a.errText(), Area: a.area.Name, Channels: len(a.area.Channels),
				GearLost: len(devices) == 0 && now.Sub(a.started) > 10*time.Second,
				Devices:  devices, Decks: a.tracker.Decks(now, s),
				Lights: a.lights(now, ph, s),
				Link:   link, Phrase: phrase, Viz: a.viz(now, ph, s),
			},
		})
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
