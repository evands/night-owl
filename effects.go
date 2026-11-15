package main

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RGB components are 0..1.
type RGB struct{ R, G, B float64 }

func (c RGB) scale(v float64) RGB { return RGB{c.R * v, c.G * v, c.B * v} }

func lerp(a, b RGB, f float64) RGB {
	return RGB{a.R + (b.R-a.R)*f, a.G + (b.G-a.G)*f, a.B + (b.B-a.B)*f}
}

func parseHex(s string) (RGB, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return RGB{}, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return RGB{}, false
	}
	return RGB{float64(v>>16&0xff) / 255, float64(v>>8&0xff) / 255, float64(v&0xff) / 255}, true
}

func hsv(h, s, v float64) RGB {
	h = math.Mod(math.Mod(h, 360)+360, 360) / 60
	i := math.Floor(h)
	f := h - i
	p, q, t := v*(1-s), v*(1-s*f), v*(1-s*(1-f))
	switch int(i) {
	case 0:
		return RGB{v, t, p}
	case 1:
		return RGB{q, v, p}
	case 2:
		return RGB{p, v, t}
	case 3:
		return RGB{p, q, v}
	case 4:
		return RGB{t, p, v}
	default:
		return RGB{v, p, q}
	}
}

// colours builds the list the show steps through, from either the hue
// range or the hand-picked palette.
func colours(s Settings) []RGB {
	if s.ColorMode == "palette" {
		out := make([]RGB, 0, len(s.Palette))
		for _, h := range s.Palette {
			if c, ok := parseHex(h); ok {
				out = append(out, c)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	start, end := s.HueStart, s.HueEnd
	if end < start {
		end += 360 // always sweep upward, wrapping through red
	}
	n := s.RangeSteps
	if n < 1 {
		n = 1
	}
	out := make([]RGB, n)
	for i := range out {
		f := 0.0
		if n > 1 {
			f = float64(i) / float64(n-1)
		}
		out[i] = hsv(start+(end-start)*f, s.Saturation, 1)
	}
	return out
}

// sample reads the colour list at a fractional, wrapping position.
func sample(cols []RGB, pos float64) RGB {
	n := len(cols)
	if n == 1 {
		return cols[0]
	}
	fl := math.Floor(pos)
	i0 := int(mod(int64(fl), int64(n)))
	return lerp(cols[i0], cols[(i0+1)%n], pos-fl)
}

func smooth(x float64) float64 { return x * x * (3 - 2*x) }

// Channel is one independently controllable light (or light segment), at
// its position in the Hue Entertainment area: X left(-1)..right(+1),
// Y back(-1)..front(+1), Z floor(-1)..ceiling(+1).
type Channel struct {
	ID      uint8
	X, Y, Z float64
	LightID string // the Hue light this channel belongs to
	Name    string // that light's name
	Slow    bool   // not in the streamed area: driven by ordinary light commands
}

// place is where a channel sits along the chosen direction of travel.
type place struct {
	Rank  int     // position group along the axis, 0..groups-1
	Order float64 // Rank scaled to 0..1
	Zone  int     // quarter of the room along the axis, 0..3
	Side  int     // half of the room along the axis, 0 or 1
}

type layoutKey struct {
	axis   string
	rev    bool
	ox, oy float64
}

// layout orders the channels along an axis of the room, or by distance
// from a point (the DJ booth) for "radial". Lights at the same spot along
// the axis share a place, so they move together.
func layout(chans []Channel, k layoutKey) []place {
	coord := make([]float64, len(chans))
	for i, c := range chans {
		switch k.axis {
		case "y":
			coord[i] = c.Y
		case "z":
			coord[i] = c.Z
		case "radial":
			coord[i] = math.Hypot(c.X-k.ox, c.Y-k.oy)
		default:
			coord[i] = c.X
		}
	}
	idx := make([]int, len(chans))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return coord[idx[a]] < coord[idx[b]] })

	const same = 0.03 // closer than this along the axis counts as one spot
	group := make([]int, len(chans))
	groups := 0
	for n, i := range idx {
		if n > 0 && coord[i]-coord[idx[n-1]] > same {
			groups++
		}
		group[i] = groups
	}
	groups++

	out := make([]place, len(chans))
	for i := range chans {
		g := group[i]
		if k.rev {
			g = groups - 1 - g
		}
		p := place{Rank: g, Zone: g * 4 / groups, Side: g * 2 / groups}
		if groups > 1 {
			p.Order = float64(g) / float64(groups-1)
		}
		out[i] = p
	}
	return out
}

// Engine turns a musical position into a colour for every channel.
type Engine struct {
	chans []Channel
	start time.Time

	mu     sync.Mutex
	key    layoutKey
	places []place
}

func NewEngine(chans []Channel) *Engine {
	return &Engine{chans: chans, start: time.Now()}
}

func (e *Engine) layoutFor(s Settings) []place {
	k := layoutKey{axis: s.Axis, rev: s.Reverse}
	if s.Axis == "radial" {
		k.ox, k.oy = s.OriginX, s.OriginY
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.places == nil || k != e.key {
		e.key, e.places = k, layout(e.chans, k)
	}
	return e.places
}

var autoCycle = []string{"pulse", "wave", "alternate", "sparkle", "chase", "sides", "bounce"}

var bounceZones = [6]int64{0, 1, 2, 3, 2, 1}

// scatter is a repeatable "random" 0..99 for a light on a beat.
func scatter(rank int, beatIdx int64) int64 {
	h := uint32(rank+1)*0x9E3779B1 ^ uint32(beatIdx+1)*0x85EBCA77
	h ^= h >> 15
	h *= 0x2C1B3C6D
	h ^= h >> 12
	h *= 0x297A2D39
	h ^= h >> 15
	return int64(h % 100)
}

// blocks says which block of `size` beats the music is in and how many
// beats into it. A new block always starts at a phrase boundary when the
// track's structure is known; failing that, blocks are counted from the
// start of the track; failing that, from when the deck was first heard.
func blocks(ph Phase, size int64) (idx int64, into float64) {
	if size < 1 {
		size = 1
	}
	if ph.PhraseKnown && ph.Structure != nil {
		p := ph.Structure.Phrases
		for i := 0; i < ph.PhraseIndex && i+1 < len(p); i++ {
			if l := int64(p[i+1].Beat - p[i].Beat); l > 0 {
				idx += (l + size - 1) / size
			}
		}
		return idx + ph.PhraseBeat/size, float64(ph.PhraseBeat%size) + ph.Frac
	}
	b := ph.BeatIdx
	if ph.TrackBeat > 0 {
		b = ph.TrackBeat - 1
	}
	return (b - mod(b, size)) / size, float64(mod(b, size)) + ph.Frac
}

func effectFor(s Settings, ph Phase) string {
	if s.Effect != "auto" {
		return s.Effect
	}
	idx, _ := blocks(ph, 4*int64(s.AutoBars))
	return autoCycle[mod(idx, int64(len(autoCycle)))]
}

// fillState says how far through a build-up the music is (0 = none, rising
// to 1 at the bar line) and whether this beat is the hit that follows one.
// Build-ups end on the same block boundaries the colours and effects use.
//
// When the track's phrases are known they decide instead: the build-up is
// the last bar before a phrase that is more intense than the current one,
// and the hit is its first beat. Phrases that ease off get neither.
func fillState(ph Phase, s Settings) (ramp float64, impact bool, left float64) {
	if s.FillBars <= 0 || !ph.Locked {
		return 0, false, 0
	}
	if ph.PhraseKnown && ph.Structure != nil {
		p, i := ph.Structure.Phrases, ph.PhraseIndex
		if i+1 < len(p) && p[i+1].Energy > p[i].Energy {
			left = float64(p[i+1].Beat-p[i].Beat) - float64(ph.PhraseBeat) - ph.Frac
			if left > 0 && left <= 4 {
				ramp = 1 - left/4
			}
		}
		return ramp, ph.PhraseBeat == 0 && i > 0 && p[i-1].Energy < p[i].Energy, math.Max(left, 0)
	}
	size := int64(4 * s.FillBars)
	idx, into := blocks(ph, size)
	left = float64(size) - into
	if left <= 4 {
		ramp = 1 - left/4
	}
	return ramp, into < 1 && idx > 0, left
}

// isDrop reports the first beat of a phrase that is at full intensity and
// more intense than the one before it: the moment a chorus lands.
func isDrop(ph Phase) bool {
	if !ph.PhraseKnown || ph.PhraseBeat != 0 || ph.PhraseIndex == 0 || ph.Energy < 0.95 {
		return false
	}
	return ph.Structure.Phrases[ph.PhraseIndex-1].Energy < ph.Energy
}

// weight is how strongly a channel takes part in a given beat.
func weight(effect string, p place, beatIdx int64) float64 {
	const rest = 0.12
	on := true
	switch effect {
	case "alternate": // neighbouring lights trade beats
		on = mod(int64(p.Rank)+beatIdx, 2) == 0
	case "sides": // the two halves of the room trade beats
		on = int64(p.Side) == mod(beatIdx, 2)
	case "chase": // the room in four zones, one per beat of the bar
		on = int64(p.Zone) == mod(beatIdx, 4)
	case "bounce": // the lit zone runs across the room and back
		on = int64(p.Zone) == bounceZones[mod(beatIdx, 6)]
	case "sparkle": // a different scattering of lights on every beat
		on = scatter(p.Rank, beatIdx) < 40
	}
	if on {
		return 1
	}
	return rest
}

func beatLevel(beatIdx int64, s Settings) float64 {
	if mod(beatIdx, 4) == 0 {
		return 1
	}
	return s.OffbeatLevel
}

// colourPos is how far through the colour list the show is right now.
func (e *Engine) colourPos(now time.Time, ph Phase, s Settings, n float64) float64 {
	if !ph.Locked {
		// Nothing playing: drift slowly through the colours.
		return now.Sub(e.start).Seconds() / s.IdleCycleS * n
	}
	// Step every ColorBeats beats, crossfading over ColorFadeMs.
	step, intoBeats := blocks(ph, int64(s.ColorBeats))
	intoMs := intoBeats * ph.Period * 1000
	if s.ColorFadeMs > 0 && intoMs < s.ColorFadeMs {
		return float64(step) - 1 + smooth(intoMs/s.ColorFadeMs)
	}
	return float64(step)
}

// Theme returns k colours for the visualizer: it starts from the colour the
// lights are on right now and reaches further along the chosen colours than
// the room does at any one moment, so a screen full of it is not one flat
// hue. It moves on whenever the lights do.
func (e *Engine) Theme(now time.Time, ph Phase, s Settings, k int) []RGB {
	cols := colours(s)
	n := float64(len(cols))
	pos := e.colourPos(now, ph, s, n)
	span := math.Max(s.ColorSpread, 0.75) * (n - 1)
	out := make([]RGB, k)
	for i := range out {
		out[i] = sample(cols, pos+span*float64(i)/float64(k-1))
	}
	return out
}

func (e *Engine) Render(now time.Time, ph Phase, s Settings) []RGB {
	cols := colours(s)
	n := float64(len(cols))
	places := e.layoutFor(s)
	out := make([]RGB, len(e.chans))
	pos := e.colourPos(now, ph, s, n)

	if !ph.Locked {
		breath := 0.85 + 0.15*math.Sin(2*math.Pi*now.Sub(e.start).Seconds()/8)
		for i, p := range places {
			out[i] = sample(cols, pos+s.ColorSpread*p.Order*(n-1)).scale(s.IdleBrightness * breath)
		}
		return out
	}

	lo, hi := s.Floor, s.Brightness
	if lo > hi {
		lo = hi
	}
	effect := effectFor(s, ph)
	drop, hit := s.DropFlash > 0 && isDrop(ph), 1.0
	ramp, impact, _ := fillState(ph, s)
	if impact && !drop {
		drop = true // the hit after a build-up lands like a drop
	}
	// During a build-up the pulses come twice, then four times, a beat.
	rollN := 0.0
	if ramp > 0 && !drop {
		rollN = 2
		if ramp >= 0.5 {
			rollN = 4
		}
	}
	if drop {
		effect = "pulse" // the whole room hits together
	} else {
		// Quieter sections of the track pulse less brightly.
		if ph.PhraseKnown {
			hi = lo + (hi-lo)*(1-s.EnergyFollow*(1-ph.Energy))
		}
	}
	for i, p := range places {
		// Where this light is within its own beat. For "wave" the pulse
		// reaches lights further along the room later.
		frac, beat := ph.Frac, ph.BeatIdx
		if effect == "wave" {
			if frac -= p.Order * s.WaveBeats; frac < 0 {
				frac++
				beat--
			}
		}
		// Brightness: a pulse that peaks on the beat and falls over FadeMs,
		// optionally ramping up over AttackMs before the next one.
		sinceMs := frac * ph.Period * 1000
		untilMs := (1 - frac) * ph.Period * 1000
		decay, attack := 0.0, 0.0
		if sinceMs < s.FadeMs {
			x := 1 - sinceMs/s.FadeMs
			decay = x * x
		}
		if s.AttackMs > 0 && untilMs < s.AttackMs {
			attack = 1 - untilMs/s.AttackMs
		}
		env := math.Max(
			decay*beatLevel(beat, s)*weight(effect, p, beat),
			attack*beatLevel(beat+1, s)*weight(effect, p, beat+1))
		col := sample(cols, pos+s.ColorSpread*p.Order*(n-1))
		top := hi
		if drop {
			env = decay
			white := s.DropFlash
			if impact {
				white = math.Max(white, 0.85)
				top = hi + (1-hi)*0.6 // and brighter than an ordinary beat
			}
			col = lerp(col, RGB{1, 1, 1}, white*hit*decay) // a white hit that settles into the colour
		} else if rollN > 0 {
			sub := ph.Frac * rollN
			sf := sub - math.Floor(sub)
			roll := 0.0
			if ms, fade := sf*ph.Period*1000/rollN, math.Max(s.FadeMs/rollN, 40); ms < fade {
				x := 1 - ms/fade
				roll = x * x
			}
			// neighbouring lights trade the quick pulses, all of them brightening
			if mod(int64(p.Rank)+int64(math.Floor(sub))+ph.BeatIdx*int64(rollN), 2) != 0 {
				roll *= 0.45
			}
			env = roll * (0.55 + 0.45*ramp)
			top = hi + (1-hi)*0.6*ramp
			col = lerp(col, RGB{1, 1, 1}, 0.5*ramp)
		}
		out[i] = col.scale(lo + (top-lo)*env)
	}
	return out
}
