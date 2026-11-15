package main

import (
	"math"
	"testing"
)

func TestShowMergesAreasIntoOneRoom(t *testing.T) {
	main := Area{ID: "a", Name: "Main", Channels: []Channel{
		{ID: 0, X: -1, LightID: "L1", Name: "Left"}, {ID: 1, X: 0, LightID: "L2", Name: "Lamp"}, {ID: 2, X: 0.1, LightID: "L2", Name: "Lamp"},
	}, Lights: []AreaLight{{ID: "L1", Name: "Left", X: -1}, {ID: "L2", Name: "Lamp", X: 0.05}}}
	booth := Area{ID: "b", Name: "Booth", Channels: []Channel{
		{ID: 0, X: 1, LightID: "L3", Name: "Booth"}, {ID: 1, X: 0.05, LightID: "L2", Name: "Lamp"},
	}, Lights: []AreaLight{{ID: "L3", Name: "Booth", X: 1}, {ID: "L2", Name: "Lamp", X: 0.05}}}
	other := Area{ID: "c", Name: "Elsewhere", Channels: []Channel{{ID: 0, LightID: "L9"}}, Lights: []AreaLight{{ID: "L9"}}}

	sh, err := buildShow([]Area{booth, main, other}, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if sh.stream.ID != "a" || len(sh.streamCh) != 3 {
		t.Fatalf("the area with the most channels should be streamed: %q with %d", sh.stream.Name, len(sh.streamCh))
	}
	if len(sh.followers) != 1 || sh.followers[0].light.ID != "L3" {
		t.Fatalf("only the booth light is new; the shared lamp is already streamed: %+v", sh.followers)
	}
	if len(sh.engine.chans) != 4 || !sh.engine.chans[3].Slow || sh.engine.chans[3].X != 1 {
		t.Fatalf("the follower should sit in the same room layout at its own position: %+v", sh.engine.chans)
	}
	// one layout across both areas: the booth light is the right-hand end of a left-to-right sweep
	places := sh.engine.layoutFor(defaultSettings())
	if places[3].Order != 1 || places[0].Order != 0 {
		t.Fatalf("left-to-right order should span both areas: %+v", places)
	}
	if _, err := buildShow([]Area{main}, []string{"zzz"}); err == nil {
		t.Fatal("a selection with no known area must be refused")
	}
}

func TestFollowersTakeTurns(t *testing.T) {
	var all []Channel
	sh := &Show{}
	for i := 0; i < 5; i++ { // five followers, placed right to left in the list
		sh.followers = append(sh.followers, follower{light: AreaLight{ID: string(rune('a' + i))}, idx: i})
		all = append(all, Channel{X: 1 - float64(i)*0.5, LightID: string(rune('a' + i)), Slow: true})
	}
	sh.engine = NewEngine(all)
	s := defaultSettings() // direction of travel: left to right
	ph := Phase{Locked: true, Period: 60.0 / 128}
	var seq []int
	for beat := int64(0); beat < 10; beat++ {
		ph.BeatIdx = beat
		turn := sh.followerTurn(ph, s)
		if len(turn) != 1 {
			t.Fatalf("one follower a beat by default, got %v", turn)
		}
		seq = append(seq, turn[0])
	}
	// one follower per beat, sweeping left to right (the last in the list is leftmost), then round again
	want := []int{4, 3, 2, 1, 0, 4, 3, 2, 1, 0}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("turns were %v, expected %v", seq, want)
		}
	}
	// two commands a flash must stay inside the budget at any tempo
	for _, bpm := range []float64{90, 128, 150, 175, 200, 260} {
		period := 60 / bpm
		if step := sh.followerStep(period, s); 2/(float64(step)*period) > followBudget+1e-9 {
			t.Errorf("at %.0f BPM a flash every %d beat(s) is over the command budget", bpm, step)
		}
	}
	if len((&Show{}).followerTurn(ph, s)) != 0 {
		t.Fatal("no followers, no turns")
	}

	// The option: two a beat, half the room apart, every follower still getting its turn.
	s.FollowerPairs = true
	seen := map[int]int{}
	for beat := int64(0); beat < 5; beat++ {
		ph.BeatIdx = beat
		turn := sh.followerTurn(ph, s)
		if len(turn) != 2 || turn[0] == turn[1] {
			t.Fatalf("beat %d: expected two different followers, got %v", beat, turn)
		}
		seen[turn[0]]++
		seen[turn[1]]++
	}
	if len(seen) != 5 {
		t.Fatalf("over five beats every follower should have flashed: %v", seen)
	}
	if ph.BeatIdx = 0; sh.followerTurn(ph, s)[1] != 1 { // leftmost (4) pairs with the one three places along (1)
		t.Fatalf("the second of the pair should be half the room away, got %v", sh.followerTurn(ph, s))
	}
	for _, bpm := range []float64{90, 128, 150, 175, 200, 260} {
		period := 60 / bpm
		if step := sh.followerStep(period, s); 4/(float64(step)*period) > pairBudget+1e-9 {
			t.Errorf("at %.0f BPM two followers every %d beat(s) is over the bridge's limit", bpm, step)
		}
	}
	if sh.followerStep(60.0/128, s) != 1 {
		t.Error("at 128 BPM the pair should flash on every beat")
	}
	one := &Show{followers: sh.followers[:1], engine: NewEngine(all[:1])}
	if got := one.followerTurn(Phase{Locked: true, Period: 0.5}, s); len(got) != 1 {
		t.Fatalf("a single follower cannot pair with itself: %v", got)
	}
}

func TestColourToXY(t *testing.T) {
	for _, c := range []struct {
		rgb  RGB
		x, y float64
	}{{RGB{1, 0, 0}, 0.640, 0.330}, {RGB{0, 1, 0}, 0.300, 0.600}, {RGB{0, 0, 1}, 0.150, 0.060}, {RGB{1, 1, 1}, 0.3127, 0.3290}} {
		x, y := xy(c.rgb)
		if math.Abs(x-c.x) > 0.005 || math.Abs(y-c.y) > 0.005 {
			t.Errorf("%+v -> (%.3f, %.3f), expected (%.3f, %.3f)", c.rgb, x, y, c.x, c.y)
		}
	}
}
