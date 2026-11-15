package main

import (
	"math"
	"testing"
	"time"
)

// A real 13-channel Entertainment area, as a bridge reports it.
var room = []Channel{
	{ID: 0, X: -1.00, Y: 0.26, Z: -1.00}, {ID: 1, X: 1.00, Y: -0.26, Z: -1.00}, {ID: 2, X: -1.00, Y: 0.10, Z: 1.00}, {ID: 3, X: -0.16, Y: 1.00, Z: 1.00},
	{ID: 4, X: -0.12, Y: 1.00, Z: -0.61}, {ID: 5, X: 0.40, Y: 0.80, Z: -0.40}, {ID: 6, X: 0.40, Y: 0.80, Z: 0.00}, {ID: 7, X: 0.40, Y: 0.80, Z: 0.40},
	{ID: 8, X: -0.40, Y: 0.80, Z: -0.40}, {ID: 9, X: -0.40, Y: 0.80, Z: 0.00}, {ID: 10, X: -0.40, Y: 0.80, Z: 0.40}, {ID: 11, X: 0.00, Y: -0.80, Z: 0.00},
	{ID: 12, X: 0.80, Y: -0.80, Z: 0.00},
}

func TestLayoutFollowsAxis(t *testing.T) {
	x := layout(room, layoutKey{axis: "x"})
	if x[0].Order != 0 || x[2].Order != 0 { // both lights at x=-1 share the far-left place
		t.Fatalf("leftmost lights should be at order 0, got %v and %v", x[0].Order, x[2].Order)
	}
	if x[1].Order != 1 {
		t.Fatalf("rightmost light should be at order 1, got %v", x[1].Order)
	}
	if x[5].Rank != x[6].Rank || x[6].Rank != x[7].Rank {
		t.Fatalf("segments of one lamp share an x and must move together: %v %v %v", x[5], x[6], x[7])
	}
	if x[0].Side != 0 || x[1].Side != 1 {
		t.Fatalf("sides should split left/right: %v %v", x[0], x[1])
	}

	y := layout(room, layoutKey{axis: "y"})
	if y[11].Order != 0 || y[3].Order != 1 { // back wall light first, front strip last
		t.Fatalf("back-to-front order wrong: back %v front %v", y[11].Order, y[3].Order)
	}

	rev := layout(room, layoutKey{axis: "x", rev: true})
	if rev[0].Order != 1 || rev[1].Order != 0 {
		t.Fatalf("reverse should flip the order: %v %v", rev[0].Order, rev[1].Order)
	}

	// Radiating from the right-hand back light: it is nearest, the far-left ones furthest.
	rad := layout(room, layoutKey{axis: "radial", ox: 0.8, oy: -0.8})
	if rad[12].Order != 0 {
		t.Fatalf("light at the origin should be first, got %v", rad[12].Order)
	}
	if rad[0].Order <= rad[11].Order {
		t.Fatalf("far light should come after a near one: %v vs %v", rad[0].Order, rad[11].Order)
	}
}

func bright(c RGB) float64 { return max(c.R, c.G, c.B) }

func TestWaveReachesFarLightsLater(t *testing.T) {
	s := defaultSettings()
	s.Effect, s.Axis, s.WaveBeats, s.FadeMs, s.ColorSpread = "wave", "x", 0.5, 150, 0
	e := NewEngine(room)
	ph := Phase{Locked: true, BPM: 120, Period: 0.5, BeatIdx: 8}

	ph.Frac = 0.02 // just after the beat: the left edge is lit, the right edge is not yet
	out := e.Render(time.Now(), ph, s)
	if !(bright(out[0]) > 0.8 && bright(out[1]) < 0.3) {
		t.Fatalf("at the beat: left %.2f should be bright, right %.2f dark", bright(out[0]), bright(out[1]))
	}
	ph.Frac = 0.52 // half a beat later the wave has arrived on the right
	out = e.Render(time.Now(), ph, s)
	if !(bright(out[1]) > 0.5 && bright(out[0]) < 0.3) {
		t.Fatalf("half a beat on: right %.2f should be bright, left %.2f faded", bright(out[1]), bright(out[0]))
	}
}

func TestSidesTradeBeats(t *testing.T) {
	s := defaultSettings()
	s.Effect, s.Axis, s.OffbeatLevel = "sides", "x", 1
	e := NewEngine(room)
	ph := Phase{Locked: true, BPM: 120, Period: 0.5, Frac: 0.01}
	ph.BeatIdx = 4
	a := e.Render(time.Now(), ph, s)
	ph.BeatIdx = 5
	b := e.Render(time.Now(), ph, s)
	if !(bright(a[0]) > bright(a[1]) && bright(b[1]) > bright(b[0])) {
		t.Fatalf("halves should swap: beat 4 left %.2f right %.2f, beat 5 left %.2f right %.2f",
			bright(a[0]), bright(a[1]), bright(b[0]), bright(b[1]))
	}
}

func TestOldSettingsFileGetsNewDefaults(t *testing.T) {
	s := defaultSettings()
	s.Axis, s.Effect = "", "nonsense"
	s.sanitize()
	if s.Axis != "x" || s.Effect != "auto" {
		t.Fatalf("sanitize should repair missing/unknown values, got axis %q effect %q", s.Axis, s.Effect)
	}
}

func TestBuildUpAndHit(t *testing.T) {
	s := defaultSettings()
	s.Effect, s.FillBars, s.OffbeatLevel, s.ColorSpread, s.Floor, s.Brightness = "pulse", 4, 1, 0, 0.1, 0.8
	e := NewEngine(room)
	at := func(beat int64, frac float64) Phase {
		return Phase{Locked: true, BPM: 120, Period: 0.5, BeatIdx: beat, Frac: frac, Energy: 1}
	}
	if r, hit, left := fillState(at(11, 0.5), s); r != 0 || hit || left != 4.5 {
		t.Fatalf("before the last bar: ramp %v hit %v left %v", r, hit, left)
	}
	if r, _, _ := fillState(at(14, 0), s); r != 0.5 {
		t.Fatalf("half way through the last bar the ramp should be 0.5, got %v", r)
	}
	// Late in the build-up the pulses come four times a beat: bright just
	// after each quarter of the beat, dark just before the next.
	on, off := bright(e.Render(time.Now(), at(15, 0.51), s)[0]), bright(e.Render(time.Now(), at(15, 0.74), s)[0])
	onB, offB := bright(e.Render(time.Now(), at(15, 0.51), s)[1]), 0.0
	if !(math.Max(on, onB) > 0.6 && off < 0.25) {
		t.Fatalf("roll should flash on the quarter-beat: on %.2f/%.2f, off %.2f (%v)", on, onB, off, offB)
	}
	// The hit: first beat of the next block, near white, whole room.
	hit := e.Render(time.Now(), at(16, 0.01), s)
	for i, c := range hit {
		if min(c.R, c.G, c.B) < 0.5 {
			t.Fatalf("light %d should be close to white on the hit, got %+v", i, c)
		}
	}
	if _, isHit, _ := fillState(at(0, 0.01), s); isHit {
		t.Fatal("the very first beat heard from a deck must not count as a hit")
	}
}

func TestNewEffectsLightOnlyPartOfTheRoom(t *testing.T) {
	for _, eff := range []string{"sparkle", "bounce"} {
		s := defaultSettings()
		s.Effect, s.FillBars, s.OffbeatLevel, s.Floor = eff, 0, 1, 0
		e := NewEngine(room)
		changed := false
		var prev []bool
		for beat := int64(4); beat < 12; beat++ {
			out := e.Render(time.Now(), Phase{Locked: true, BPM: 120, Period: 0.5, BeatIdx: beat, Frac: 0.01, Energy: 1}, s)
			lit, n := make([]bool, len(out)), 0
			for i, c := range out {
				if lit[i] = bright(c) > 0.5; lit[i] {
					n++
				}
			}
			if n == 0 || n == len(out) {
				t.Fatalf("%s beat %d: %d of %d lights lit; expected some but not all", eff, beat, n, len(out))
			}
			if prev != nil {
				for i := range lit {
					changed = changed || lit[i] != prev[i]
				}
			}
			prev = lit
		}
		if !changed {
			t.Fatalf("%s should light different lights on different beats", eff)
		}
	}
}

// A machine on both a cable and Wi-Fi hears each beat twice; the late copy
// must not disturb the clock or show up as jitter.
func TestEchoedBeatPacketsAreIgnored(t *testing.T) {
	c := &deckClock{player: 1}
	t0 := time.Now()
	for i := 0; i < 40; i++ {
		at := t0.Add(time.Duration(i) * 500 * time.Millisecond)
		p := BeatPacket{Player: 1, At: at, BPM: 120, BeatInBar: i%4 + 1}
		c.update(p)
		p.At = at.Add(time.Duration(40+13*(i%9)) * time.Millisecond) // the Wi-Fi copy, 40-144 ms late
		c.update(p)
	}
	if c.spread() > 0.005 {
		t.Fatalf("echoes leaked into the clock: jitter reads %.0f ms", c.spread()*1000)
	}
	if d := c.anchor.Sub(t0.Add(39 * 500 * time.Millisecond)); d < -2*time.Millisecond || d > 2*time.Millisecond {
		t.Fatalf("grid drifted by %v", d)
	}
	if c.beatCount != 39 {
		t.Fatalf("counted %d beats, expected 39", c.beatCount)
	}
}
