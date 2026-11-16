package main

import (
	"encoding/binary"
	"math"
	"net"
	"testing"
	"time"
)

// announcement builds a Link discovery message the way a real program
// sends it: header, then timeline, session and measurement-port entries.
func announcement(kind, ttl byte, node, session linkID, tl linkTimeline, measurePort int) []byte {
	b := append([]byte{}, linkDiscoveryMagic...)
	b = append(b, kind, ttl, 0, 0)
	b = append(b, node[:]...)
	entry := func(key uint32, v []byte) {
		h := make([]byte, 8)
		binary.BigEndian.PutUint32(h, key)
		binary.BigEndian.PutUint32(h[4:], uint32(len(v)))
		b = append(append(b, h...), v...)
	}
	t := make([]byte, 24)
	binary.BigEndian.PutUint64(t, uint64(tl.microsPerBeat))
	binary.BigEndian.PutUint64(t[8:], uint64(tl.beatOrigin))
	binary.BigEndian.PutUint64(t[16:], uint64(tl.timeOrigin))
	entry(keyTimeline, t)
	entry(keySession, session[:])
	entry(0x73747374, make([]byte, 17)) // 'stst': start/stop state, which we skip
	if measurePort > 0 {
		m := []byte{192, 168, 1, 50, 0, 0}
		binary.BigEndian.PutUint16(m[4:], uint16(measurePort))
		entry(keyMeasure4, m)
	}
	return b
}

func TestLinkAnnouncementParses(t *testing.T) {
	node, session := linkID{1, 2, 3, 4, 5, 6, 7, 8}, linkID{9, 9, 9, 9, 9, 9, 9, 9}
	tl := linkTimeline{microsPerBeat: 500000, beatOrigin: 12_000_000, timeOrigin: 3_000_000}
	m, err := parseLinkAnnouncement(announcement(linkAlive, 5, node, session, tl, 4321))
	if err != nil {
		t.Fatal(err)
	}
	if m.kind != linkAlive || m.ttl != 5*time.Second || m.node != node || m.session != session || m.tl != tl || m.measurePort != 4321 || !m.hasTimeline || !m.hasSession {
		t.Fatalf("parsed wrongly: %+v", m)
	}
	if got := m.tl.bpm(); math.Abs(got-120) > 1e-9 {
		t.Fatalf("500000 microseconds a beat is 120 BPM, got %v", got)
	}
	// One second after the origin at 120 BPM is two beats on from beat 12.
	if got := tl.beats(4_000_000); math.Abs(got-14) > 1e-9 {
		t.Fatalf("beat at one second past the origin: %v", got)
	}
	for _, bad := range [][]byte{nil, []byte("_asdp_v\x01"), append([]byte("_link_v\x01"), make([]byte, 30)...)} {
		if _, err := parseLinkAnnouncement(bad); err == nil {
			t.Fatalf("%q must be refused", bad)
		}
	}
	// An entry that claims to be longer than the message must not be read.
	cut := announcement(linkAlive, 5, node, session, tl, 4321)
	if _, err := parseLinkAnnouncement(cut[:len(cut)-3]); err == nil {
		t.Fatal("a truncated message must be refused")
	}
	// A nonsense tempo is not a timeline to follow.
	if m, _ := parseLinkAnnouncement(announcement(linkAlive, 5, node, session, linkTimeline{microsPerBeat: 10}, 0)); m.hasTimeline {
		t.Fatal("a tempo of millions of BPM must be ignored")
	}
}

func TestLinkChoosesAndLeavesSessions(t *testing.T) {
	a := NewAbletonLink()
	now := time.Now()
	from := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 50), Port: 50123}
	hear := func(kind byte, node, session linkID, tl linkTimeline, at time.Time) {
		t.Helper()
		m, err := parseLinkAnnouncement(announcement(kind, 5, node, session, tl, 4321))
		if err != nil {
			t.Fatal(err)
		}
		a.heard(m, from, at)
	}
	rb, live, phone := linkID{1}, linkID{2}, linkID{3}
	s1, s2 := linkID{0xaa}, linkID{0xbb}
	slow, fast := linkTimeline{500000, 0, 0}, linkTimeline{480000, 8_000_000, 100}

	hear(linkAlive, rb, s1, slow, now)
	if !a.inSession || a.session != s1 || a.tl != slow {
		t.Fatalf("the only session heard must be followed: %+v", a)
	}
	// The address to ping is where the message came from, with the port it named.
	if p := a.peers[rb]; p.measure == nil || !p.measure.IP.Equal(from.IP) || p.measure.Port != 4321 {
		t.Fatalf("ping address: %+v", p.measure)
	}
	// No clock measured yet, so no beat to report.
	if _, ok := a.State(now); ok {
		t.Fatal("no beat before the clock has been measured")
	}
	// A tempo change arrives as a timeline with a later beat origin.
	hear(linkAlive, live, s1, fast, now)
	if a.tl != fast {
		t.Fatalf("the newer timeline must win: %+v", a.tl)
	}
	hear(linkAlive, rb, s1, slow, now) // a stale repeat must not undo it
	if a.tl != fast {
		t.Fatal("an older timeline must not replace a newer one")
	}
	// A lone program in another session does not pull us away from two.
	hear(linkAlive, phone, s2, slow, now)
	if a.session != s1 {
		t.Fatal("the session with more programs must be kept")
	}
	// Goodbyes and silence empty the session, and then the other is followed.
	hear(linkByeBye, live, s1, fast, now)
	later := now.Add(6 * time.Second)
	hear(linkAlive, phone, s2, slow, later) // rb has now been silent past its 5 s
	if a.session != s2 || len(a.peers) != 1 {
		t.Fatalf("after the others left, the remaining session: %x with %d", a.session, len(a.peers))
	}
	hear(linkByeBye, phone, s2, slow, later)
	if a.inSession {
		t.Fatal("no programs left: no session")
	}
}

// fakeLinkPeer answers pings the way a Link program does: its session,
// its reading of the shared clock, and our own values echoed back.
func fakeLinkPeer(t *testing.T, session linkID, ghost func() int64) *net.UDPAddr {
	t.Helper()
	con, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skipf("cannot open a local socket: %v", err)
	}
	t.Cleanup(func() { con.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := con.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n < 9 || string(buf[:8]) != string(linkPingMagic) || buf[8] != linkPing {
				continue
			}
			out := append(append([]byte{}, linkPingMagic...), linkPong)
			h := make([]byte, 8)
			binary.BigEndian.PutUint32(h, keySession)
			binary.BigEndian.PutUint32(h[4:], 8)
			out = append(append(out, h...), session[:]...)
			out = append(out, linkEntry(keyGhostTime, ghost())...)
			out = append(out, buf[9:n]...) // the ping's own entries, echoed
			_, _ = con.WriteToUDP(out, from)
		}
	}()
	return con.LocalAddr().(*net.UDPAddr)
}

func TestLinkMeasuresTheClockAndFollowsTheBeat(t *testing.T) {
	session := linkID{0xaa}
	// The session's clock runs 5000 s ahead of the peer's own start time.
	peerEpoch := time.Now().Add(-90 * time.Second)
	const ahead = 5_000_000_000
	ghostAt := func(at time.Time) int64 { return at.Sub(peerEpoch).Microseconds() + ahead }
	addr := fakeLinkPeer(t, session, func() int64 { return ghostAt(time.Now()) })

	a := NewAbletonLink()
	con, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skipf("cannot open a local socket: %v", err)
	}
	defer con.Close()
	a.pingCon = con

	// 125 BPM, with beat 100 at the session clock's 5000 s mark.
	tl := linkTimeline{microsPerBeat: 480000, beatOrigin: 100_000_000, timeOrigin: ahead}
	m, _ := parseLinkAnnouncement(announcement(linkAlive, 5, linkID{1}, session, tl, addr.Port))
	a.heard(m, &net.UDPAddr{IP: addr.IP, Port: 40000}, time.Now())

	to, s, due := a.target(time.Now())
	if !due || s != session || to.Port != addr.Port {
		t.Fatalf("a measurement must be due against the program heard: %v %x %v", to, s, due)
	}
	data, err := a.measure(to, s)
	if err != nil || len(data) < 100 {
		t.Fatalf("measurement: %v, %d samples", err, len(data))
	}
	want := float64(a.epoch.Sub(peerEpoch).Microseconds() + ahead)
	if got := median(data); math.Abs(got-want) > 1000 {
		t.Fatalf("clock offset off by %.0f microseconds", got-want)
	}
	a.offset, a.offsetFor, a.haveOffset = median(data), s, true

	at := time.Now().Add(3 * time.Second)
	st, ok := a.State(at)
	if !ok || st.Peers != 1 || math.Abs(st.BPM-125) > 1e-9 {
		t.Fatalf("state: %+v %v", st, ok)
	}
	wantBeat := 100 + float64(ghostAt(at)-ahead)/480000
	if math.Abs(st.Beat-wantBeat) > 0.005 { // 0.005 beats is 2.4 ms at 125 BPM
		t.Fatalf("beat %.4f, want %.4f", st.Beat, wantBeat)
	}
	// A program in another session must not be measured as this one.
	if _, err := a.measure(to, linkID{0xbb}); err == nil {
		t.Fatal("an answer from another session must fail the measurement")
	}

	// The tracker turns that into the phase the lights use, cued early by
	// the light lead, and counts bars from the session's multiples of four.
	tr := NewTracker(NewDJLink())
	tr.ab = a
	set := defaultSettings()
	set.LeadMs = 0
	ph := tr.Phase(at, set)
	fl := math.Floor(st.Beat)
	if !ph.Locked || ph.Source != "link" || ph.BeatIdx != int64(fl) || ph.BeatInBar != int(mod(int64(fl), 4))+1 || math.Abs(ph.Frac-(st.Beat-fl)) > 1e-6 || math.Abs(ph.Period-0.48) > 1e-9 {
		t.Fatalf("phase from Link: %+v (beat %.4f)", ph, st.Beat)
	}
	set.LeadMs = 96 // a fifth of a beat at 125 BPM
	early := tr.Phase(at, set)
	if d := (float64(early.BeatIdx) + early.Frac) - (float64(ph.BeatIdx) + ph.Frac); math.Abs(d-0.2) > 1e-6 {
		t.Fatalf("the light lead must move the beat forward by 0.2 beats, moved %.4f", d)
	}
	set.BeatSource = "decks"
	if tr.Phase(at, set).Locked {
		t.Fatal("with decks only, Link must be ignored")
	}
	set.BeatSource = "link"
	if !tr.Phase(at, set).Locked {
		t.Fatal("with Link only, the session must be followed")
	}
}
