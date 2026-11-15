package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Settings holds everything that shapes the light show. Every field is
// editable live from the control page and is saved to the config file.
type Settings struct {
	// Timing
	LeadMs    float64 `json:"lead_ms"`    // fire lights this early to cancel Hue lag
	FadeMs    float64 `json:"fade_ms"`    // time for a beat pulse to fall back to the floor
	AttackMs  float64 `json:"attack_ms"`  // ramp-up before the beat (0 = instant)
	StopBeats float64 `json:"stop_beats"` // beats of silence before a deck counts as stopped

	// Brightness (0..1)
	Brightness   float64 `json:"brightness"`    // peak of a pulse
	Floor        float64 `json:"floor"`         // level between pulses
	OffbeatLevel float64 `json:"offbeat_level"` // strength of beats 2-4 relative to beat 1

	// Movement
	Effect    string  `json:"effect"`     // pulse | wave | alternate | sides | chase | sparkle | bounce | auto
	AutoBars  int     `json:"auto_bars"`  // bars per effect when effect = auto
	Axis      string  `json:"axis"`       // direction effects and colours travel: x | y | z | radial
	Reverse   bool    `json:"reverse"`    // travel the other way along the axis
	OriginX   float64 `json:"origin_x"`   // radial: where the pulse starts (the DJ booth), -1..1
	OriginY   float64 `json:"origin_y"`   // radial: -1 back .. +1 front
	WaveBeats float64 `json:"wave_beats"` // wave: beats the pulse takes to cross the room

	// Colour
	ColorMode   string   `json:"color_mode"`    // range | palette
	HueStart    float64  `json:"hue_start"`     // degrees, range mode
	HueEnd      float64  `json:"hue_end"`       // degrees, range mode (sweeps upward from start)
	Saturation  float64  `json:"saturation"`    // 0..1, range mode
	RangeSteps  int      `json:"range_steps"`   // colours picked across the hue range
	Palette     []string `json:"palette"`       // hex colours, palette mode
	ColorBeats  int      `json:"color_beats"`   // beats between colour changes
	ColorFadeMs float64  `json:"color_fade_ms"` // crossfade time when the colour changes
	ColorSpread float64  `json:"color_spread"`  // 0 = whole room one colour, 1 = full range across the room

	// Idle (nothing playing)
	IdleBrightness float64 `json:"idle_brightness"`
	IdleCycleS     float64 `json:"idle_cycle_s"` // seconds to drift through the colours

	// Deck selection
	FollowOnAir bool `json:"follow_on_air"` // only follow decks whose mixer channel is live

	// Deck link: announce on the DJ network as a player to receive deck
	// status and track structure. Off = listen only.
	DeckLink     bool    `json:"deck_link"`
	EnergyFollow float64 `json:"energy_follow"` // 0..1: how much quiet sections dim the pulses
	DropFlash    float64 `json:"drop_flash"`    // 0..1: white hit on the first beat of a chorus

	// Build-ups: the last bar of every FillBars bars rolls faster and
	// brighter, and the next downbeat lands as a white hit. 0 = off.
	FillBars int `json:"fill_bars"`

	// Visualizer
	VizBars int `json:"viz_bars"` // bars between scene changes; 0 = only by hand
}

func defaultSettings() Settings {
	return Settings{
		// Measured on a wired PC from slow-motion video of the lights against
		// the mixer's meters and the kick (bridge BSB002, 13 channels): 100 ms
		// with fixed 20 ms frames, less the ~10 ms that beat-exact frames save.
		LeadMs: 90, FadeMs: 350, AttackMs: 0, StopBeats: 3.5,
		Brightness: 1.0, Floor: 0.12, OffbeatLevel: 0.7,
		Effect: "auto", AutoBars: 8, Axis: "x", WaveBeats: 0.5,
		ColorMode: "range", HueStart: 200, HueEnd: 340, Saturation: 1.0, RangeSteps: 6,
		Palette:    []string{"#ff0040", "#7a00ff", "#0060ff", "#00e0ff"},
		ColorBeats: 16, ColorFadeMs: 400, ColorSpread: 0.35,
		IdleBrightness: 0.25, IdleCycleS: 30,
		FollowOnAir: true,
		DeckLink:    false, EnergyFollow: 0.6, DropFlash: 0.7,
		VizBars:  16,
		FillBars: 16,
	}
}

func clampF(v, lo, hi float64) float64 {
	if v != v { // NaN
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampI(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// sanitize forces every value into a range the renderer can rely on.
func (s *Settings) sanitize() {
	d := defaultSettings()
	s.LeadMs = clampF(s.LeadMs, -200, 500)
	s.FadeMs = clampF(s.FadeMs, 20, 4000)
	s.AttackMs = clampF(s.AttackMs, 0, 1000)
	s.StopBeats = clampF(s.StopBeats, 1.5, 16)
	s.Brightness = clampF(s.Brightness, 0, 1)
	s.Floor = clampF(s.Floor, 0, 1)
	s.OffbeatLevel = clampF(s.OffbeatLevel, 0, 1)
	switch s.Effect {
	case "pulse", "wave", "alternate", "sides", "chase", "sparkle", "bounce", "auto":
	default:
		s.Effect = d.Effect
	}
	switch s.Axis {
	case "x", "y", "z", "radial":
	default:
		s.Axis = d.Axis
	}
	s.OriginX = clampF(s.OriginX, -1, 1)
	s.OriginY = clampF(s.OriginY, -1, 1)
	s.WaveBeats = clampF(s.WaveBeats, 0, 1)
	s.AutoBars = clampI(s.AutoBars, 1, 64)
	if s.ColorMode != "range" && s.ColorMode != "palette" {
		s.ColorMode = d.ColorMode
	}
	s.HueStart = clampF(s.HueStart, 0, 360)
	s.HueEnd = clampF(s.HueEnd, 0, 360)
	s.Saturation = clampF(s.Saturation, 0, 1)
	s.RangeSteps = clampI(s.RangeSteps, 1, 24)
	var pal []string
	for _, c := range s.Palette {
		if _, ok := parseHex(c); ok && len(pal) < 12 {
			pal = append(pal, c)
		}
	}
	if len(pal) == 0 {
		pal = d.Palette
	}
	s.Palette = pal
	s.ColorBeats = clampI(s.ColorBeats, 1, 256)
	s.ColorFadeMs = clampF(s.ColorFadeMs, 0, 10000)
	s.ColorSpread = clampF(s.ColorSpread, 0, 1)
	s.IdleBrightness = clampF(s.IdleBrightness, 0, 1)
	s.IdleCycleS = clampF(s.IdleCycleS, 2, 600)
	s.EnergyFollow = clampF(s.EnergyFollow, 0, 1)
	s.DropFlash = clampF(s.DropFlash, 0, 1)
	s.VizBars = clampI(s.VizBars, 0, 256)
	s.FillBars = clampI(s.FillBars, 0, 256)
}

// Config is the on-disk file: bridge pairing plus the settings.
type Config struct {
	BridgeIP  string   `json:"bridge_ip"`
	BridgeID  string   `json:"bridge_id"` // stable id, used to re-find the bridge if its address changes
	Username  string   `json:"username"`
	ClientKey string   `json:"clientkey"`
	AreaID    string   `json:"area_id"`
	AreaName  string   `json:"area_name"`
	Settings  Settings `json:"settings"`
	Presets   []Preset `json:"presets"`
	Preset    string   `json:"preset"` // name of the preset last applied or saved
}

// Store guards the config and persists changes.
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

// loadStore reads the settings file. If it does not exist yet, it adopts
// the first legacy file that does (settings once lived beside the program)
// and saves a copy at the new location.
func loadStore(path string, legacy ...string) (*Store, error) {
	st := &Store{path: path, cfg: Config{Settings: defaultSettings()}}
	for i, p := range append([]string{path}, legacy...) {
		data, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &st.cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		st.cfg.Settings.sanitize()
		if st.cfg.seedPresets(true) || i > 0 {
			if err := st.saveLocked(); err != nil {
				return nil, err
			}
		}
		return st, nil
	}
	st.cfg.seedPresets(false) // a new installation; saved with the first change
	return st, nil
}

func (st *Store) Config() Config {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.cfg
}

func (st *Store) Settings() Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.cfg.Settings
}

func (st *Store) SetSettings(s Settings) (Settings, error) {
	s.sanitize()
	st.mu.Lock()
	st.cfg.Settings = s
	err := st.saveLocked()
	st.mu.Unlock()
	return s, err
}

func (st *Store) Update(fn func(c *Config)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	fn(&st.cfg)
	return st.saveLocked()
}

func (st *Store) saveLocked() error {
	data, err := json.MarshalIndent(st.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(st.path, data, 0o600)
}
