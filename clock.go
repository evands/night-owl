package main

import (
	"log"
	"math"
	"sort"
	"sync"
	"time"
)

// A deckClock turns a stream of beat packets into a steady beat grid.
//
// The tempo comes from the packet itself (track BPM x pitch), so only the
// phase has to be estimated. Packets can only ever arrive late, never early,
// and over Wi-Fi they arrive up to ~100 ms late or not at all (broadcasts are
// held for the access point's beacon). So the grid tracks the EARLIEST recent
// arrivals: a packet that lands before the grid pulls the grid earlier at
// once, and a sliding minimum lets it drift later again. Between packets the
// grid free-runs, so lost packets do not cost beats.
type deckClock struct {
	player    int
	locked    bool
	lockedAt  time.Time
	bpm       float64
	period    float64   // seconds per beat
	anchor    time.Time // estimated true time of the most recent announced beat
	beatInBar int       // bar position (1..4) of the anchor beat
	beatCount int64     // running beat index of the anchor beat; beatCount%4 == beatInBar-1
	lastPkt   time.Time
	resid     []float64 // recent arrival delays relative to the grid, seconds
	bad       int       // consecutive packets that did not fit the grid

	// From deck status, when the deck link is on: how to turn our running
	// beat index into the beat number within the loaded track.
	trackKnown bool
	trackOff   int64
	trackKey   TrackKey
	hasKey     bool
}

const residWindow = 12

func mod(a, n int64) int64 { return ((a % n) + n) % n }

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

func (c *deckClock) relock(p BeatPacket, period float64) {
	c.locked = true
	c.lockedAt = p.At
	c.bpm, c.period = p.BPM, period
	c.anchor = p.At
	c.beatInBar = p.BeatInBar
	c.beatCount = int64(p.BeatInBar - 1)
	c.lastPkt = p.At
	c.resid = c.resid[:0]
	c.bad = 0
	c.trackKnown = false // the beat index restarted; the next status packet re-anchors it
}

func (c *deckClock) shift(d float64) {
	c.anchor = c.anchor.Add(secs(d))
	for i := range c.resid {
		c.resid[i] -= d
	}
}

func (c *deckClock) update(p BeatPacket) {
	period := 60 / p.BPM
	if !c.locked || p.At.Sub(c.lastPkt).Seconds() > 8*period {
		c.relock(p, period)
		return
	}
	// A machine on both a cable and Wi-Fi hears every packet twice, the
	// Wi-Fi copy late. Keep the first copy of each beat and drop the echo.
	if p.BeatInBar == c.beatInBar && p.At.Sub(c.lastPkt).Seconds() < 0.5*period {
		return
	}
	c.lastPkt = p.At
	c.bpm, c.period = p.BPM, period

	// How many beats since the anchor? The bar position pins it modulo 4,
	// which survives lost packets and heavy jitter.
	el := p.At.Sub(c.anchor).Seconds()
	delta := float64(mod(int64(p.BeatInBar-c.beatInBar), 4))
	n := delta + 4*math.Round((el/period-delta)/4)
	r := el - n*period // how late this packet is relative to the grid

	if r < -0.15 || r > 0.25 {
		// Does not fit: a cue, loop, jump or scratch. One stray is ignored;
		// two in a row means the track really moved.
		c.bad++
		if c.bad >= 2 {
			c.relock(p, period)
		}
		return
	}
	c.bad = 0
	c.anchor = c.anchor.Add(secs(n * period))
	c.beatCount += int64(n)
	c.beatInBar = p.BeatInBar

	c.resid = append(c.resid, r)
	if len(c.resid) > residWindow {
		c.resid = c.resid[len(c.resid)-residWindow:]
	}
	switch {
	case r < 0:
		c.shift(r) // arrived before the grid: the grid was late
	case len(c.resid) >= 6:
		// Every recent packet was late by at least m: the grid is early
		// (the deck was nudged back, or the tempo drifted).
		m := c.resid[0]
		for _, v := range c.resid {
			m = math.Min(m, v)
		}
		if m > 0 {
			c.shift(m)
		}
	}
}

// spread is the widest recent arrival delay: ~0 on a wired link, up to
// ~100 ms on Wi-Fi.
func (c *deckClock) spread() float64 {
	m := 0.0
	for _, v := range c.resid {
		m = math.Max(m, v)
	}
	return m
}

// Phase is where the music is at one instant.
type Phase struct {
	Locked    bool
	Player    int
	BPM       float64
	Period    float64 // seconds per beat
	Frac      float64 // 0..1 through the current beat
	BeatIdx   int64   // running beat index; multiples of 4 are downbeats
	BeatInBar int     // 1..4

	// Known only with the deck link on.
	TrackBeat   int64   // beat number within the track, 1-based; 0 if unknown
	PhraseKnown bool    // the track has phrase analysis and we are inside it
	PhraseLabel string  // Intro, Verse 1, Chorus, ...
	PhraseBeat  int64   // beats since the phrase began
	Energy      float64 // 0..1 for the current phrase; 1 when unknown
	Structure   *Structure
	PhraseIndex int
}

// DeckStatus is a per-deck summary for the control page.
type DeckStatus struct {
	Player   int     `json:"player"`
	BPM      float64 `json:"bpm"`
	Playing  bool    `json:"playing"`
	OnAir    bool    `json:"on_air"`
	Master   bool    `json:"master"`
	JitterMs float64 `json:"jitter_ms"`
}

// Tracker keeps a clock per deck and decides which deck the lights follow.
type Tracker struct {
	mu      sync.Mutex
	dj      *DJLink
	link    *VCDJ        // nil-safe: deck status when the deck link is on
	phrases *PhraseStore // track structures fetched from the decks
	clocks  map[int]*deckClock
	active  int
}

func NewTracker(dj *DJLink) *Tracker {
	return &Tracker{dj: dj, clocks: map[int]*deckClock{}}
}

// Run consumes beat packets until the channel closes.
func (t *Tracker) Run() {
	for p := range t.dj.Beats {
		t.mu.Lock()
		c := t.clocks[p.Player]
		if c == nil {
			c = &deckClock{player: p.Player}
			t.clocks[p.Player] = c
		}
		c.update(p)
		t.mu.Unlock()
	}
}

// OnStatus ties a deck's beat grid to beat numbers within its track. It is
// only trusted mid-beat, where the status packet and the grid cannot
// disagree about which beat is current.
func (t *Tracker) OnStatus(st DeckState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.clocks[st.Player]
	if c == nil || !c.locked {
		return
	}
	c.trackKey, c.hasKey = st.trackKey()
	if st.BeatNumber <= 0 || !st.Playing {
		return
	}
	pos := st.At.Sub(c.anchor).Seconds() / c.period
	fl := math.Floor(pos)
	if f := pos - fl; f < 0.2 || f > 0.8 {
		return
	}
	c.trackOff = st.BeatNumber - (c.beatCount + int64(fl))
	c.trackKnown = true
}

func deckOnAir(player int, flags [4]bool, fresh bool) bool {
	if !fresh || player < 1 || player > 4 {
		return true // no mixer on the link: treat every deck as live
	}
	return flags[player-1]
}

// deckFacts merges what is known about a deck: status packets when the deck
// link is on (exact and immediate), otherwise beat packets and the mixer.
func (t *Tracker) deckFacts(c *deckClock, now time.Time, s Settings, flags [4]bool, fresh bool) (playing, onAir, master bool) {
	playing = c.locked && now.Sub(c.lastPkt).Seconds() <= s.StopBeats*c.period
	onAir = deckOnAir(c.player, flags, fresh)
	if t.link != nil {
		if st, ok := t.link.State(c.player, now); ok {
			// Beat packets are the proof a deck is playing: a deck that keeps
			// sending them is followed whatever its status says. Status only
			// shortens the wait once the beats have stopped arriving, so a
			// stopped deck is dropped after one missed beat instead of several.
			if !st.Playing && now.Sub(c.lastPkt).Seconds() > 1.25*c.period {
				playing = false
			}
			master = st.Master
			if !fresh {
				onAir = st.OnAir
			}
		}
	}
	return
}

// Phase picks the deck to follow and reports the musical position at
// now+lead. With the deck link on, the tempo master leads whenever it is
// playing on air, which is what a DJ means by "the deck in charge" during a
// blend. Otherwise a deck stays chosen until it stops or leaves the air.
func (t *Tracker) Phase(now time.Time, s Settings) Phase {
	flags, fresh := t.dj.OnAir(now)
	t.mu.Lock()
	defer t.mu.Unlock()

	var best, current, lead *deckClock
	for _, c := range t.clocks {
		playing, onAir, master := t.deckFacts(c, now, s, flags, fresh)
		if !playing || (s.FollowOnAir && !onAir) {
			continue
		}
		if master {
			lead = c
		}
		if c.player == t.active {
			current = c
		}
		if best == nil || c.lockedAt.After(best.lockedAt) {
			best = c
		}
	}
	switch {
	case lead != nil:
		best = lead
	case current != nil:
		best = current
	}
	if best == nil {
		if t.active != 0 {
			log.Printf("no deck playing on air: lights idle")
			t.active = 0
		}
		return Phase{}
	}
	if best.player != t.active {
		log.Printf("following deck %d at %.2f BPM", best.player, best.bpm)
		t.active = best.player
	}

	at := now.Add(secs(s.LeadMs / 1000))
	pos := at.Sub(best.anchor).Seconds() / best.period
	fl := math.Floor(pos)
	ph := Phase{
		Locked:    true,
		Player:    best.player,
		BPM:       best.bpm,
		Period:    best.period,
		Frac:      pos - fl,
		BeatIdx:   best.beatCount + int64(fl),
		BeatInBar: int(mod(int64(best.beatInBar-1)+int64(fl), 4)) + 1,
		Energy:    1,
	}
	if best.trackKnown && t.link != nil {
		if _, ok := t.link.State(best.player, now); ok {
			ph.TrackBeat = ph.BeatIdx + best.trackOff
			if best.hasKey && t.phrases != nil {
				if st := t.phrases.Lookup(best.trackKey); st != nil {
					if p, i, into, ok := st.At(ph.TrackBeat); ok {
						ph.PhraseKnown, ph.PhraseLabel, ph.Energy = true, p.Label, p.Energy
						ph.PhraseBeat, ph.Structure, ph.PhraseIndex = into, st, i
					}
				}
			}
		}
	}
	return ph
}

// Decks summarises every deck heard from recently.
func (t *Tracker) Decks(now time.Time, s Settings) []DeckStatus {
	flags, fresh := t.dj.OnAir(now)
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []DeckStatus{}
	for _, c := range t.clocks {
		if !c.locked || now.Sub(c.lastPkt) > 30*time.Second {
			continue
		}
		playing, onAir, master := t.deckFacts(c, now, s, flags, fresh)
		out = append(out, DeckStatus{
			Player:   c.player,
			BPM:      math.Round(c.bpm*100) / 100,
			Playing:  playing,
			OnAir:    onAir,
			Master:   master,
			JitterMs: math.Round(c.spread() * 1000),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Player < out[j].Player })
	return out
}
