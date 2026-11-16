package main

import (
	"errors"
	"reflect"
	"strings"
)

// A preset is a saved look: colours, effect, brightness, pulse shape and so
// on. It deliberately leaves out what belongs to the machine and the room
// rather than the look: the light lead (a latency calibration), how decks
// are chosen, whether to join the DJ network, and how follower lights are
// paced.
type Preset struct {
	Name string   `json:"name"`
	Look Settings `json:"look"`
}

// lookOf blanks the machine-specific fields, leaving only the look.
func lookOf(s Settings) Settings {
	s.LeadMs, s.StopBeats, s.FollowOnAir, s.DeckLink, s.FollowerPairs, s.BeatSource = 0, 0, false, false, false, ""
	return s
}

// withLook returns cur with the look replaced and its machine fields kept.
func withLook(cur, look Settings) Settings {
	look.LeadMs, look.StopBeats, look.FollowOnAir, look.DeckLink = cur.LeadMs, cur.StopBeats, cur.FollowOnAir, cur.DeckLink
	look.FollowerPairs, look.BeatSource = cur.FollowerPairs, cur.BeatSource
	look.Palette = append([]string(nil), look.Palette...)
	look.sanitize()
	return look
}

func sameLook(a, b Settings) bool { return reflect.DeepEqual(lookOf(a), lookOf(b)) }

// builtinPresets are the looks a new installation starts with. Each is
// built around colours that sit well together.
func builtinPresets() []Preset {
	mk := func(name string, edit func(s *Settings)) Preset {
		s := defaultSettings()
		edit(&s)
		s.sanitize()
		return Preset{Name: name, Look: lookOf(s)}
	}
	return []Preset{
		mk("Classic", func(s *Settings) {}), // blue through violet to magenta, effects rotating
		mk("Blue-Green Chase", func(s *Settings) { // cool blues into greens, a chase running right to left
			s.ColorMode, s.Palette = "palette", []string{"#0060ff", "#00e0ff", "#00fa1d", "#114d00"}
			s.Effect, s.Reverse, s.ColorBeats, s.ColorFadeMs, s.ColorSpread = "chase", true, 4, 250, 0.7
			s.Brightness, s.Floor, s.OffbeatLevel = 0.75, 0.4, 0.5
		}),
		mk("Sunset Wave", func(s *Settings) { // neighbours on the warm side: rose, vermilion, orange, gold
			s.ColorMode, s.Palette = "palette", []string{"#ff2e63", "#ff5a1f", "#ff9a00", "#ffd23f"}
			s.Effect, s.WaveBeats, s.ColorBeats, s.ColorFadeMs, s.ColorSpread = "wave", 0.6, 16, 800, 0.6
			s.Brightness, s.Floor, s.OffbeatLevel, s.FadeMs = 0.9, 0.15, 0.7, 450
		}),
		mk("Neon Nights", func(s *Settings) { // a split-complementary trio: cyan against violet and hot pink
			s.ColorMode, s.Palette = "palette", []string{"#00e5ff", "#7b2cff", "#ff2bd6"}
			s.Effect, s.ColorBeats, s.ColorFadeMs, s.ColorSpread = "alternate", 8, 200, 0.5
			s.Brightness, s.Floor, s.OffbeatLevel, s.FadeMs, s.FillBars = 1, 0.06, 0.85, 280, 8
		}),
		mk("Deep Ocean", func(s *Settings) { // one cool family, teal to deep blue, drifting slowly
			s.ColorMode, s.HueStart, s.HueEnd, s.RangeSteps = "range", 170, 255, 6
			s.Effect, s.ColorBeats, s.ColorFadeMs, s.ColorSpread = "bounce", 16, 1200, 0.8
			s.Brightness, s.Floor, s.OffbeatLevel, s.FadeMs, s.FillBars = 0.8, 0.22, 0.6, 500, 32
		}),
		mk("Ember Glow", func(s *Settings) { // crimson through red to amber, a heavy downbeat
			s.ColorMode, s.HueStart, s.HueEnd, s.RangeSteps = "range", 350, 35, 5
			s.Effect, s.ColorBeats, s.ColorFadeMs, s.ColorSpread = "pulse", 32, 1500, 0.35
			s.Brightness, s.Floor, s.OffbeatLevel, s.FadeMs, s.AttackMs = 0.95, 0.12, 0.45, 550, 60
		}),
		mk("Aurora Sparkle", func(s *Settings) { // green through cyan to violet, scattered across the room
			s.ColorMode, s.HueStart, s.HueEnd, s.RangeSteps = "range", 110, 285, 8
			s.Effect, s.ColorBeats, s.ColorFadeMs, s.ColorSpread = "sparkle", 4, 300, 1
			s.Brightness, s.Floor, s.OffbeatLevel, s.FadeMs = 0.95, 0.08, 0.9, 320
		}),
		mk("Candy Shop", func(s *Settings) { // five evenly spaced pastels, everything rotating quickly
			s.ColorMode, s.Palette = "palette", []string{"#ff4fa3", "#ffd166", "#06d6a0", "#4cc9f0", "#b388ff"}
			s.Effect, s.AutoBars, s.ColorBeats, s.ColorFadeMs, s.ColorSpread = "auto", 4, 8, 400, 0.9
			s.Brightness, s.Floor, s.OffbeatLevel, s.FillBars = 0.9, 0.12, 0.75, 8
		}),
	}
}

// seedPresets gives a config its starting presets, and keeps whatever look
// it already had under a name of its own if that look is not one of them.
// It reports whether anything changed.
func (c *Config) seedPresets(existing bool) bool {
	if len(c.Presets) > 0 {
		return false
	}
	c.Presets = builtinPresets()
	c.Preset = ""
	for _, p := range c.Presets {
		if sameLook(p.Look, c.Settings) {
			c.Preset = p.Name
		}
	}
	if c.Preset == "" && existing {
		c.Presets = append(c.Presets, Preset{Name: "My settings", Look: lookOf(c.Settings)})
		c.Preset = "My settings"
	}
	return true
}

func (c *Config) presetIndex(name string) int {
	for i, p := range c.Presets {
		if strings.EqualFold(p.Name, name) {
			return i
		}
	}
	return -1
}

func cleanName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", errors.New("a preset needs a name")
	}
	if len([]rune(name)) > 40 {
		return "", errors.New("preset names can be up to 40 characters")
	}
	return name, nil
}

// PresetInfo is what the control page needs to draw the preset menu.
type PresetInfo struct {
	Names    []string `json:"names"`
	Active   string   `json:"active"`
	Modified bool     `json:"modified"` // the current look differs from the active preset
}

func (st *Store) PresetInfo() PresetInfo {
	st.mu.RLock()
	defer st.mu.RUnlock()
	info := PresetInfo{Names: []string{}, Active: st.cfg.Preset}
	for _, p := range st.cfg.Presets {
		info.Names = append(info.Names, p.Name)
	}
	if i := st.cfg.presetIndex(st.cfg.Preset); i >= 0 {
		info.Modified = !sameLook(st.cfg.Presets[i].Look, st.cfg.Settings)
	} else {
		info.Active = ""
	}
	return info
}

// PresetAction applies, saves, renames or deletes a preset.
func (st *Store) PresetAction(action, name, to string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	c := &st.cfg
	switch action {
	case "apply":
		i := c.presetIndex(name)
		if i < 0 {
			return errors.New("no preset called " + name)
		}
		c.Settings = withLook(c.Settings, c.Presets[i].Look)
		c.Preset = c.Presets[i].Name
	case "save": // the current look, under this name (new, or replacing)
		name, err := cleanName(name)
		if err != nil {
			return err
		}
		look := lookOf(c.Settings)
		look.Palette = append([]string(nil), look.Palette...)
		if i := c.presetIndex(name); i >= 0 {
			c.Presets[i].Look = look
			name = c.Presets[i].Name
		} else {
			if len(c.Presets) >= 60 {
				return errors.New("that is a lot of presets: delete one first")
			}
			c.Presets = append(c.Presets, Preset{Name: name, Look: look})
		}
		c.Preset = name
	case "rename":
		to, err := cleanName(to)
		if err != nil {
			return err
		}
		i := c.presetIndex(name)
		if i < 0 {
			return errors.New("no preset called " + name)
		}
		if j := c.presetIndex(to); j >= 0 && j != i {
			return errors.New("there is already a preset called " + c.Presets[j].Name)
		}
		if c.Preset == c.Presets[i].Name {
			c.Preset = to
		}
		c.Presets[i].Name = to
	case "delete":
		i := c.presetIndex(name)
		if i < 0 {
			return errors.New("no preset called " + name)
		}
		if c.Preset == c.Presets[i].Name {
			c.Preset = "" // the lights keep the look; it just no longer has a name
		}
		c.Presets = append(c.Presets[:i], c.Presets[i+1:]...)
	default:
		return errors.New("unknown preset action " + action)
	}
	return st.saveLocked()
}
