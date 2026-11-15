package main

import (
	"path/filepath"
	"testing"
)

func TestPresets(t *testing.T) {
	st, err := loadStore(filepath.Join(t.TempDir(), "cdjhue.json"))
	if err != nil {
		t.Fatal(err)
	}
	info := st.PresetInfo()
	if len(info.Names) < 6 || info.Active != "Classic" || info.Modified {
		t.Fatalf("a new installation should start on Classic with the built-in looks: %+v", info)
	}
	// Machine calibration survives a preset change; the look does not.
	s := st.Settings()
	s.LeadMs, s.FollowOnAir, s.DeckLink, s.FollowerPairs = 123, false, true, true
	if _, err := st.SetSettings(s); err != nil {
		t.Fatal(err)
	}
	if err := st.PresetAction("apply", "sunset wave", ""); err != nil { // names match whatever the capitals
		t.Fatal(err)
	}
	s = st.Settings()
	if s.LeadMs != 123 || s.FollowOnAir || !s.DeckLink || !s.FollowerPairs {
		t.Fatalf("applying a preset must not touch lead or deck options: %+v", s)
	}
	if s.Effect != "wave" || s.ColorMode != "palette" || len(s.Palette) != 4 || st.PresetInfo().Active != "Sunset Wave" {
		t.Fatalf("Sunset Wave did not apply: %+v", s)
	}
	// Edit, save under a new name, rename, delete.
	s.Brightness = 0.33
	st.SetSettings(s)
	if !st.PresetInfo().Modified {
		t.Fatal("an edit should mark the preset as modified")
	}
	if err := st.PresetAction("save", "  Dim   Sunset ", ""); err != nil {
		t.Fatal(err)
	}
	if info := st.PresetInfo(); info.Active != "Dim Sunset" || info.Modified {
		t.Fatalf("after save as: %+v", info)
	}
	if err := st.PresetAction("rename", "Dim Sunset", "Classic"); err == nil {
		t.Fatal("renaming onto an existing preset must be refused")
	}
	if err := st.PresetAction("rename", "Dim Sunset", "Dusk"); err != nil || st.PresetInfo().Active != "Dusk" {
		t.Fatalf("rename: %v, %+v", err, st.PresetInfo())
	}
	st.PresetAction("apply", "Classic", "")
	if st.Settings().Brightness == 0.33 {
		t.Fatal("Classic should have restored its own brightness")
	}
	st.PresetAction("apply", "Dusk", "")
	if st.Settings().Brightness != 0.33 {
		t.Fatal("Dusk should bring its brightness back")
	}
	if err := st.PresetAction("delete", "Dusk", ""); err != nil {
		t.Fatal(err)
	}
	if info := st.PresetInfo(); info.Active != "" || st.Settings().Brightness != 0.33 {
		t.Fatalf("deleting the active preset keeps the look but drops the name: %+v", info)
	}
	// Everything is on disk: a fresh load sees the same presets.
	again, err := loadStore(st.path)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := again.PresetInfo().Names, st.PresetInfo().Names; len(a) != len(b) {
		t.Fatalf("presets not saved: %v vs %v", a, b)
	}
	// Built-in looks must be valid and all different.
	bs := builtinPresets()
	for i, p := range bs {
		q := p.Look
		q.sanitize()
		if !sameLook(q, p.Look) {
			t.Errorf("%s is changed by sanitising, so it holds an out-of-range value", p.Name)
		}
		for _, o := range bs[:i] {
			if sameLook(o.Look, p.Look) {
				t.Errorf("%s and %s are the same look", o.Name, p.Name)
			}
		}
	}
}

// Upgrading: a settings file from before presets keeps its look under a name.
func TestUpgradeKeepsExistingLook(t *testing.T) {
	c := Config{Settings: defaultSettings()}
	c.Settings.Effect, c.Settings.Brightness = "sides", 0.61
	if !c.seedPresets(true) || c.Preset != "My settings" {
		t.Fatalf("the existing look should be kept as My settings, got %q", c.Preset)
	}
	if i := c.presetIndex("My settings"); i < 0 || c.Presets[i].Look.Effect != "sides" {
		t.Fatal("My settings does not hold the existing look")
	}
}
