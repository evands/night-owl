package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A Hue bridge streams to one Entertainment area at a time, and an area
// holds at most ten lights. To light a bigger room, several areas can be
// selected and are treated as one space:
//
//   - the selected area with the most channels is streamed, 50 frames a
//     second, exactly as before;
//   - lights that are only in the other selected areas are "followers".
//     They are driven with ordinary light commands, of which the bridge
//     accepts about ten a second in total, and it falls behind if several
//     arrive at once. So the followers take turns: on each beat one of them
//     flashes in the show's colour, the turn moving across the room.
//
// Every light, streamed or following, sits in one shared room layout, so
// waves, chases and the colour spread run across all of them.
type Show struct {
	engine    *Engine
	stream    Area      // the streamed area
	streamCh  []Channel // its channels, in frame order
	streamIdx []int     // where each of them sits in the engine's channels
	followers []follower
	names     []string // selected area names, the streamed one first
}

type follower struct {
	light AreaLight
	idx   int // its place in the engine's channels
}

// buildShow lays out the selected areas as one room.
func buildShow(areas []Area, selected []string) (*Show, error) {
	want := map[string]bool{}
	for _, id := range selected {
		want[id] = true
	}
	var chosen []Area
	for _, a := range areas {
		if want[a.ID] && len(a.Channels) > 0 {
			chosen = append(chosen, a)
		}
	}
	if len(chosen) == 0 {
		return nil, errors.New("none of the selected Entertainment areas is on the bridge any more")
	}
	best := 0
	for i, a := range chosen {
		if len(a.Channels) > len(chosen[best].Channels) {
			best = i
		}
	}
	sh := &Show{stream: chosen[best], names: []string{chosen[best].Name}}
	var all []Channel
	inShow := map[string]bool{}
	for i, c := range sh.stream.Channels {
		sh.streamCh = append(sh.streamCh, c)
		sh.streamIdx = append(sh.streamIdx, i)
		all = append(all, c)
		inShow[c.LightID] = true
	}
	for i, a := range chosen {
		if i == best {
			continue
		}
		sh.names = append(sh.names, a.Name)
		for _, l := range a.Lights {
			if l.ID == "" || inShow[l.ID] {
				continue // already streamed, or already following from another area
			}
			inShow[l.ID] = true
			sh.followers = append(sh.followers, follower{light: l, idx: len(all)})
			all = append(all, Channel{X: l.X, Y: l.Y, Z: l.Z, LightID: l.ID, Name: l.Name, Slow: true})
		}
	}
	sh.engine = NewEngine(all)
	return sh, nil
}

// followBudget is how many ordinary light commands a second the followers
// may use, leaving headroom under the bridge's limit of about ten.
// pairBudget is the higher figure the two-a-beat option is allowed: the
// limit itself, with nothing to spare.
const (
	followBudget = 6.0
	pairBudget   = 10.0
)

// followersPerBeat is how many followers flash together.
func (sh *Show) followersPerBeat(s Settings) int {
	if s.FollowerPairs && len(sh.followers) >= 2 {
		return 2
	}
	return 1
}

// followerStep is how many beats pass between follower flashes. A flash is
// two commands a light (up, then fade), so it is every beat until the tempo
// is fast enough to break the budget.
func (sh *Show) followerStep(period float64, s Settings) int64 {
	if len(sh.followers) == 0 || period <= 0 {
		return 0
	}
	per, budget := float64(2*sh.followersPerBeat(s)), followBudget
	if s.FollowerPairs {
		budget = pairBudget
	}
	for _, n := range []int64{1, 2, 4} {
		if per/(float64(n)*period) <= budget {
			return n
		}
	}
	return 4
}

// followerTurn says which followers flash on this beat (indexes into
// sh.followers); none if it is not a flash beat. Turns run along the
// direction of travel. With two a beat, the second is half the room away,
// so two points of light travel across together.
func (sh *Show) followerTurn(ph Phase, s Settings) []int {
	step := sh.followerStep(ph.Period, s)
	if !ph.Locked || step == 0 || mod(ph.BeatIdx, step) != 0 {
		return nil
	}
	places := sh.engine.layoutFor(s)
	n := len(sh.followers)
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return places[sh.followers[order[a]].idx].Order < places[sh.followers[order[b]].idx].Order
	})
	at := mod(ph.BeatIdx/step, int64(n))
	turn := []int{order[at]}
	if sh.followersPerBeat(s) == 2 {
		turn = append(turn, order[mod(at+int64(n+1)/2, int64(n))])
	}
	return turn
}

// followerLead is how much earlier than the streamed lights a follower's
// command is sent: an ordinary command takes longer to reach its light.
const followerLead = 80.0 // ms

// follow drives the follower lights until ctx ends, then puts them back as
// they were found.
func (sh *Show) follow(ctx context.Context, hue *Hue, app *App) {
	if len(sh.followers) == 0 {
		return
	}
	ids := make([]string, len(sh.followers))
	for i, f := range sh.followers {
		ids[i] = f.light.ID
	}
	before := hue.LightStates(ids)
	log.Printf("%d more light(s) from other areas follow by ordinary commands, taking turns to flash on the beat", len(ids))

	// A small pool sends the commands. One that has waited too long is
	// dropped rather than sent late: a late flash is worse than none.
	type job struct {
		at time.Time
		do func()
	}
	jobs := make(chan job, 64)
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if time.Since(j.at) < 250*time.Millisecond {
					j.do()
				}
			}
		}()
	}
	// set sends one light command, reporting the first failure and then
	// only every hundredth, so a persistent problem is visible but quiet.
	var failed atomic.Int64
	set := func(id string, c RGB, bri float64, fade time.Duration) {
		if err := hue.SetLight(id, c, bri, fade); err != nil {
			if n := failed.Add(1); n == 1 || n%100 == 0 {
				log.Printf("a follower light did not take its command (%d so far): %v", n, err)
			}
		}
	}
	send := func(do func()) {
		select {
		case jobs <- job{at: time.Now(), do: do}:
		default: // queue full: drop
		}
	}
	defer func() {
		close(jobs)
		wg.Wait()
		for id, st := range before {
			hue.RestoreLight(id, st)
		}
	}()

	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	lastBeat := int64(math.MinInt64)
	lastIdle, idleNext := time.Time{}, 0
	wasPaused := false
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			// Paused: put the followers back as they were, once any command
			// already on its way has landed. Started again: note how they
			// are now, to put them back to that later.
			if p := app.isPaused(); p != wasPaused {
				wasPaused = p
				if p {
					time.Sleep(400 * time.Millisecond)
					for id, st := range before {
						hue.RestoreLight(id, st)
					}
				} else {
					before = hue.LightStates(ids)
				}
			}
			if wasPaused || !app.streaming.Load() {
				continue // standing by: leave the lights to whoever is driving
			}
			s := app.store.Settings()
			s.LeadMs += followerLead
			ph := app.tracker.Phase(now, s)
			if !ph.Locked {
				// Nothing playing: drift with the idle colours, one light at
				// a time so the commands are spread out.
				every := 2500 * time.Millisecond / time.Duration(len(sh.followers))
				if now.Sub(lastIdle) > every {
					lastIdle = now
					f := sh.followers[idleNext%len(sh.followers)]
					idleNext++
					c, id := sh.engine.Render(now, ph, s)[f.idx], f.light.ID
					send(func() { set(id, c, math.Max(c.R, math.Max(c.G, c.B)), 2500*time.Millisecond) })
				}
				continue
			}
			if ph.BeatIdx == lastBeat {
				continue
			}
			lastBeat = ph.BeatIdx
			turn := sh.followerTurn(ph, s)
			if len(turn) == 0 {
				continue
			}
			// Each one's flash: the show's colour for its place in the room
			// at the top of a full pulse, then a fade to the floor.
			top := ph
			top.Frac = 0
			s.Effect = "pulse" // a turn is its own; the streamed lights' pattern does not dim it
			cols := sh.engine.Render(now, top, s)
			fade := time.Duration(s.FadeMs) * time.Millisecond
			for _, t := range turn {
				f := sh.followers[t]
				c, id := cols[f.idx], f.light.ID
				peak := math.Max(c.R, math.Max(c.G, c.B))
				floor := math.Min(math.Max(s.Floor, 0.01), peak)
				send(func() { set(id, c, peak, 0) })
				time.AfterFunc(110*time.Millisecond, func() {
					if ctx.Err() == nil && !app.isPaused() {
						send(func() { set(id, c, floor, fade) })
					}
				})
			}
		}
	}
}

// describe says in a line which lights are covered and how.
func (sh *Show) describe() string {
	d := fmt.Sprintf("%q streamed (%d channels)", sh.stream.Name, len(sh.streamCh))
	if n := len(sh.followers); n > 0 {
		d += fmt.Sprintf(", plus %d light(s) following from %s", n, strings.Join(sh.names[1:], ", "))
	}
	return d
}
