package main

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// buildPSSI makes a song structure tag the way rekordbox lays it out.
func buildPSSI(mood int, phrases [][2]int, endBeat int, masked bool) []byte {
	n := len(phrases)
	b := make([]byte, 0x20+24*n)
	copy(b, "PSSI")
	binary.BigEndian.PutUint32(b[4:], 0x20)
	binary.BigEndian.PutUint32(b[8:], uint32(len(b)))
	binary.BigEndian.PutUint32(b[12:], 24)
	binary.BigEndian.PutUint16(b[16:], uint16(n))
	binary.BigEndian.PutUint16(b[18:], uint16(mood))
	binary.BigEndian.PutUint16(b[0x1a:], uint16(endBeat))
	for i, p := range phrases {
		e := b[0x20+24*i:]
		binary.BigEndian.PutUint16(e[0:], uint16(i+1))
		binary.BigEndian.PutUint16(e[2:], uint16(p[0]))
		binary.BigEndian.PutUint16(e[4:], uint16(p[1]))
	}
	if masked {
		for i := 18; i < len(b); i++ {
			b[i] ^= pssiMask[(i-18)%len(pssiMask)] + byte(n)
		}
	}
	return b
}

var houseTrack = [][2]int{{1, 1}, {65, 2}, {129, 5}, {193, 3}, {257, 2}, {321, 5}, {449, 6}} // intro up chorus down up chorus outro

func TestParsePSSI(t *testing.T) {
	for _, masked := range []bool{false, true} {
		st, err := parsePSSI(buildPSSI(1, houseTrack, 512, masked))
		if err != nil {
			t.Fatalf("masked=%v: %v", masked, err)
		}
		if st.Mood != "high" || st.EndBeat != 512 || len(st.Phrases) != 7 {
			t.Fatalf("masked=%v: got %+v", masked, st)
		}
		if st.Phrases[2].Label != "Chorus" || st.Phrases[2].Beat != 129 || st.Phrases[2].Energy != 1 {
			t.Fatalf("masked=%v: third phrase %+v", masked, st.Phrases[2])
		}
		p, i, into, ok := st.At(140)
		if !ok || p.Label != "Chorus" || i != 2 || into != 11 {
			t.Fatalf("beat 140 should be 11 beats into the chorus, got %+v %d %d %v", p, i, into, ok)
		}
	}
	// A real CDJ-2000NXS2 prefixes the tag with its length, little-endian.
	raw := buildPSSI(1, houseTrack, 512, true)
	framed := append([]byte{byte(len(raw)), byte(len(raw) >> 8), 0, 0}, raw...)
	if st, err := parsePSSI(framed); err != nil || len(st.Phrases) != 7 || st.Phrases[2].Label != "Chorus" {
		t.Fatalf("length-prefixed tag: %+v, %v", st, err)
	}
	if _, err := parsePSSI([]byte("PWV4 not a structure tag at all.....")); err == nil {
		t.Fatal("a different tag must be rejected")
	}
}

// A CDJ-2000NXS2 keep-alive as captured from a real deck (with its hardware
// and network addresses replaced); ours must have the same layout so
// players treat it as one of their own.
func TestKeepAliveMatchesRealPlayer(t *testing.T) {
	real, _ := hex.DecodeString(strings.ReplaceAll(
		"51 73 70 74 31 57 6d 4a 4f 4c 06 00 43 44 4a 2d 32 30 30 30 4e 58 53 32 00 00 00 00 00 00 00 00 "+
			"01 02 00 36 01 01 02 00 00 00 00 01 a9 fe 00 01 04 00 00 00 01 20", " ", ""))
	ours := keepAlive(4, net.HardwareAddr{0xaa, 0xbb, 0xcc, 0x01, 0x02, 0x03}, net.IPv4(169, 254, 57, 58))
	if len(ours) != len(real) {
		t.Fatalf("length %d, real is %d", len(ours), len(real))
	}
	for _, off := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 0x0a, 0x0b, 0x20, 0x21, 0x22, 0x23, 0x25, 0x34} { // fixed framing bytes
		if ours[off] != real[off] {
			t.Errorf("byte 0x%02x is %02x, a real player sends %02x", off, ours[off], real[off])
		}
	}
	if ours[0x24] != 4 || !net.IP(ours[0x2c:0x30]).Equal(net.IPv4(169, 254, 57, 58)) || ours[0x26] != 0xaa {
		t.Errorf("number, MAC or address in the wrong place: % x", ours[0x24:0x30])
	}
}

func TestParseStatus(t *testing.T) {
	p := make([]byte, 0x11c)
	copy(p, djMagic)
	p[0x0a], p[0x21] = 0x0a, 3
	p[0x28], p[0x29], p[0x2a] = 2, 3, 1 // loaded from player 2's USB, rekordbox track
	binary.BigEndian.PutUint32(p[0x2c:], 4711)
	p[0x89] = 0x40 | 0x20 | 0x08 // playing, master, on air
	binary.BigEndian.PutUint32(p[0xa0:], 345)
	p[0xa6] = 1
	st, ok := parseStatus(p, time.Now())
	if !ok || st.Player != 3 || !st.Playing || !st.Master || st.Synced || !st.OnAir || st.BeatNumber != 345 || st.TrackID != 4711 {
		t.Fatalf("got %+v", st)
	}
	if key, ok := st.trackKey(); !ok || key != (TrackKey{2, 3, 4711}) {
		t.Fatalf("track key %+v %v", key, ok)
	}
	binary.BigEndian.PutUint32(p[0xa0:], 0xffffffff)
	if st, _ := parseStatus(p, time.Now()); st.BeatNumber != 0 {
		t.Fatalf("unknown beat number should read as 0, got %d", st.BeatNumber)
	}
}

// A stand-in for a deck's database server, speaking the documented protocol.
func TestFetchPSSIAgainstFakeDeck(t *testing.T) {
	lookup, err := net.Listen("tcp4", "127.0.0.1:12523")
	if err != nil {
		t.Skip("port 12523 is in use on this machine")
	}
	defer lookup.Close()
	db, _ := net.Listen("tcp4", "127.0.0.1:0")
	defer db.Close()
	tag := buildPSSI(1, houseTrack, 512, true)
	var gotDMST, gotID, gotTag, gotExt uint32

	go func() { // port lookup
		c, err := lookup.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		q := make([]byte, 19)
		if _, err := io.ReadFull(c, q); err != nil || string(q[4:18]) != "RemoteDBServer" {
			return
		}
		port := db.Addr().(*net.TCPAddr).Port
		_, _ = c.Write([]byte{byte(port >> 8), byte(port)})
	}()
	go func() { // database server
		c, err := db.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		if v, _, _ := readField(r); v != 1 {
			return
		}
		_, _ = c.Write(num4(1))
		readHeader(r) // setup
		readField(r)
		_, _ = c.Write(dbMessage(0xfffffffe, 0x4000, 0, 2))
		readHeader(r) // tag request
		gotDMST, _, _ = readField(r)
		gotID, _, _ = readField(r)
		gotTag, _, _ = readField(r)
		gotExt, _, _ = readField(r)
		// reply 0x4f02: request type, 0, length, blob, 0
		m := append(num4(0x872349ae), num4(1)...)
		m = append(m, num2(0x4f02)...)
		m = append(m, num1(5)...)
		m = append(m, 0x14, 0, 0, 0, 12, 6, 6, 6, 3, 6, 0, 0, 0, 0, 0, 0, 0)
		m = append(m, num4(0x2c04)...)
		m = append(m, num4(0)...)
		m = append(m, num4(uint32(len(tag)))...)
		m = append(m, 0x14, byte(len(tag)>>24), byte(len(tag)>>16), byte(len(tag)>>8), byte(len(tag)))
		m = append(m, tag...)
		m = append(m, num4(0)...)
		_, _ = c.Write(m)
		readHeader(r) // goodbye
	}()

	got, err := fetchPSSI("127.0.0.1", 4, TrackKey{Source: 2, Slot: 3, ID: 4711})
	if err != nil {
		t.Fatal(err)
	}
	st, err := parsePSSI(got)
	if err != nil || len(st.Phrases) != 7 {
		t.Fatalf("parsed %+v, %v", st, err)
	}
	if gotDMST != 0x04010301 || gotID != 4711 || gotTag != 0x49535350 || gotExt != 0x00545845 {
		t.Fatalf("request was DMST %08x id %d tag %08x ext %08x", gotDMST, gotID, gotTag, gotExt)
	}
}

// With phrases known, build-ups lead into more intense phrases only.
func TestBuildUpsFollowPhrases(t *testing.T) {
	st, _ := parsePSSI(buildPSSI(1, houseTrack, 512, false)) // intro up chorus down up chorus outro
	s := defaultSettings()
	at := func(trackBeat int64, frac float64) Phase {
		ph := Phase{Locked: true, Period: 0.5, BPM: 120, BeatIdx: trackBeat - 1, TrackBeat: trackBeat, Frac: frac, Energy: 1}
		p, i, into, _ := st.At(trackBeat)
		ph.PhraseKnown, ph.PhraseLabel, ph.Energy, ph.PhraseBeat, ph.Structure, ph.PhraseIndex = true, p.Label, p.Energy, into, st, i
		return ph
	}
	if r, _, left := fillState(at(127, 0), s); r != 0.5 || left != 2 { // two beats before the chorus at 129
		t.Fatalf("into the chorus: ramp %v, left %v", r, left)
	}
	if _, hit, _ := fillState(at(129, 0.01), s); !hit {
		t.Fatal("the first beat of the chorus should be a hit")
	}
	if r, _, left := fillState(at(191, 0), s); r != 0 || left != 0 { // chorus into "down" at 193: no build
		t.Fatalf("easing off should not build: ramp %v, left %v", r, left)
	}
	if _, hit, _ := fillState(at(193, 0.01), s); hit {
		t.Fatal("a quieter phrase should not start with a hit")
	}
	if r, _, _ := fillState(at(100, 0), s); r != 0 { // mid-phrase: nothing, however long the phrase
		t.Fatalf("mid-phrase ramp %v", r)
	}
}

func TestChangesLandOnPhraseBoundaries(t *testing.T) {
	st, _ := parsePSSI(buildPSSI(1, houseTrack, 512, false))
	at := func(trackBeat int64) Phase {
		ph := Phase{Locked: true, Period: 0.5, BPM: 120, BeatIdx: trackBeat - 1, TrackBeat: trackBeat, Energy: 1}
		if p, i, into, ok := st.At(trackBeat); ok {
			ph.PhraseKnown, ph.PhraseLabel, ph.Energy, ph.PhraseBeat, ph.Structure, ph.PhraseIndex = true, p.Label, p.Energy, into, st, i
		}
		return ph
	}
	// With 48-beat blocks, a plain count would not change at beat 129 (128/48 is mid-block).
	before, _ := blocks(at(128), 48)
	after, into := blocks(at(129), 48)
	if after != before+1 || into != 0 {
		t.Fatalf("the chorus at beat 129 should start a new block: before %d, after %d, into %v", before, after, into)
	}
	if !isDrop(at(129)) || isDrop(at(130)) || isDrop(at(65)) {
		t.Fatalf("only the first beat of the chorus is the drop: %v %v %v", isDrop(at(129)), isDrop(at(130)), isDrop(at(65)))
	}

	// Energy following: the intro pulses dimmer than the chorus; the drop is white.
	s := defaultSettings()
	s.Effect, s.OffbeatLevel, s.ColorSpread, s.EnergyFollow, s.DropFlash = "pulse", 1, 0, 1, 1
	e := NewEngine(room)
	shot := func(beat int64) RGB { ph := at(beat); ph.Frac = 0.01; return e.Render(time.Now(), ph, s)[0] }
	intro, chorus, drop := shot(17), shot(141), shot(129)
	if !(bright(chorus) > bright(intro)+0.3) {
		t.Fatalf("chorus %.2f should be much brighter than intro %.2f", bright(chorus), bright(intro))
	}
	if min(drop.R, drop.G, drop.B) < 0.85 {
		t.Fatalf("the drop should be close to white, got %+v", drop)
	}
}

// What went wrong on the server: the decks' status said "not playing" for a
// deck that was plainly sending beats, and the lights went idle. Beats win.
func TestBeatsOutrankAStatusThatSaysStopped(t *testing.T) {
	dj := NewDJLink()
	tr := NewTracker(dj)
	link := NewVCDJ(dj, NewPhraseStore(), nil)
	link.active = true
	tr.link = link
	s := defaultSettings()
	now := time.Now()
	c := &deckClock{player: 3}
	for i := 0; i < 8; i++ {
		c.update(BeatPacket{Player: 3, At: now.Add(time.Duration(i-7) * 500 * time.Millisecond), BPM: 120, BeatInBar: i%4 + 1})
	}
	tr.clocks[3] = c
	link.states[3] = DeckState{Player: 3, At: now, Playing: false, Master: true, OnAir: true}
	if ph := tr.Phase(now.Add(100*time.Millisecond), s); !ph.Locked || ph.Player != 3 {
		t.Fatalf("a deck still sending beats must be followed even if its status says stopped: %+v", ph)
	}
	// Once the beats stop, that same status drops it after about one beat...
	link.states[3] = DeckState{Player: 3, At: now.Add(800 * time.Millisecond), Playing: false, Master: true, OnAir: true}
	if ph := tr.Phase(now.Add(800*time.Millisecond), s); ph.Locked {
		t.Fatal("with the beats stopped and status saying stopped, the deck should be dropped quickly")
	}
	// ...whereas without status it would still be riding out missed packets.
	tr.link = nil
	if ph := tr.Phase(now.Add(800*time.Millisecond), s); !ph.Locked {
		t.Fatal("without status the deck is kept for stop_beats beats")
	}
}
