package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"sync"
	"time"
)

// Ableton Link is a second source of beats, for DJ software that has no
// DJ Link network of its own: rekordbox in Performance mode with a USB
// controller, Serato, Traktor, Ableton Live and so on. Programs on the same
// network that have Link switched on share one tempo and one beat grid.
//
// This is a listener written from the wire format, not Ableton's library.
// It never announces itself, so it is not a member of the session and can
// never change the tempo. It hears the announcements the other programs
// send to a multicast group, and it measures the offset between its clock
// and the session's shared clock by exchanging pings with one of them.

const (
	linkGroup = "224.76.78.75"
	linkPort  = 20808
)

var (
	linkDiscoveryMagic = []byte{'_', 'a', 's', 'd', 'p', '_', 'v', 1}
	linkPingMagic      = []byte{'_', 'l', 'i', 'n', 'k', '_', 'v', 1}
)

const (
	linkAlive, linkResponse, linkByeBye = 1, 2, 3
	linkPing, linkPong                  = 1, 2

	keyTimeline  = 0x746d6c6e // 'tmln'
	keySession   = 0x73657373 // 'sess'
	keyMeasure4  = 0x6d657034 // 'mep4'
	keyHostTime  = 0x5f5f6874 // '__ht'
	keyGhostTime = 0x5f5f6774 // '__gt'
	keyPrevGhost = 0x5f706774 // '_pgt'
)

// linkTimeline maps the session's shared clock to beats. Times are in
// microseconds of that clock, beats in millionths of a beat.
type linkTimeline struct {
	microsPerBeat, beatOrigin, timeOrigin int64
}

func (tl linkTimeline) bpm() float64 { return 60e6 / float64(tl.microsPerBeat) }

// beats is the session's beat position at a time on the shared clock.
// Every program in the session counts its bars from the same place, so the
// whole part modulo four is the place in the bar for all of them.
func (tl linkTimeline) beats(ghost float64) float64 {
	return float64(tl.beatOrigin)/1e6 + (ghost-float64(tl.timeOrigin))/float64(tl.microsPerBeat)
}

type linkID [8]byte

type linkPeer struct {
	session linkID
	tl      linkTimeline
	measure *net.UDPAddr // where it answers pings; nil if it did not say
	seen    time.Time
	ttl     time.Duration
}

// linkAnnouncement is one parsed discovery message.
type linkAnnouncement struct {
	kind        byte
	ttl         time.Duration
	node        linkID
	session     linkID
	tl          linkTimeline
	hasTimeline bool
	hasSession  bool
	measurePort int
}

// linkEntries walks the key/size/value entries that follow a message header.
func linkEntries(b []byte, each func(key uint32, value []byte)) error {
	for len(b) > 0 {
		if len(b) < 8 {
			return errors.New("truncated entry header")
		}
		key, size := binary.BigEndian.Uint32(b), int(binary.BigEndian.Uint32(b[4:]))
		b = b[8:]
		if size < 0 || size > len(b) {
			return errors.New("entry longer than the message")
		}
		each(key, b[:size])
		b = b[size:]
	}
	return nil
}

func parseLinkAnnouncement(b []byte) (linkAnnouncement, error) {
	var a linkAnnouncement
	if len(b) < 20 || !bytes.Equal(b[:8], linkDiscoveryMagic) {
		return a, errors.New("not a Link announcement")
	}
	a.kind, a.ttl = b[8], time.Duration(b[9])*time.Second
	copy(a.node[:], b[12:20])
	err := linkEntries(b[20:], func(key uint32, v []byte) {
		switch {
		case key == keyTimeline && len(v) == 24:
			a.tl = linkTimeline{int64(binary.BigEndian.Uint64(v)), int64(binary.BigEndian.Uint64(v[8:])), int64(binary.BigEndian.Uint64(v[16:]))}
			a.hasTimeline = a.tl.microsPerBeat > 0 && a.tl.bpm() >= 20 && a.tl.bpm() <= 999
		case key == keySession && len(v) == 8:
			copy(a.session[:], v)
			a.hasSession = true
		case key == keyMeasure4 && len(v) == 6:
			a.measurePort = int(binary.BigEndian.Uint16(v[4:]))
		}
	})
	return a, err
}

func linkEntry(key uint32, value int64) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint32(b, key)
	binary.BigEndian.PutUint32(b[4:], 8)
	binary.BigEndian.PutUint64(b[8:], uint64(value))
	return b
}

// linkPingMessage is a ping carrying our clock and, after the first, the
// session clock reading from the previous answer.
func linkPingMessage(host, prevGhost int64) []byte {
	b := append(append([]byte{}, linkPingMagic...), linkPing)
	b = append(b, linkEntry(keyHostTime, host)...)
	if prevGhost != 0 {
		b = append(b, linkEntry(keyPrevGhost, prevGhost)...)
	}
	return b
}

type linkPongMessage struct {
	session                    linkID
	ghost, prevGhost, prevHost int64
}

func parseLinkPong(b []byte) (linkPongMessage, error) {
	var p linkPongMessage
	if len(b) < 9 || !bytes.Equal(b[:8], linkPingMagic) || b[8] != linkPong {
		return p, errors.New("not a Link pong")
	}
	err := linkEntries(b[9:], func(key uint32, v []byte) {
		if key == keySession && len(v) == 8 {
			copy(p.session[:], v)
		}
		if len(v) != 8 {
			return
		}
		switch n := int64(binary.BigEndian.Uint64(v)); key {
		case keyGhostTime:
			p.ghost = n
		case keyPrevGhost:
			p.prevGhost = n
		case keyHostTime:
			p.prevHost = n
		}
	})
	return p, err
}

// AbletonLink listens for a Link session and reports its beat.
type AbletonLink struct {
	mu        sync.Mutex
	epoch     time.Time // zero of our own clock
	peers     map[linkID]*linkPeer
	session   linkID
	tl        linkTimeline
	inSession bool

	offset      float64 // session clock minus our clock, in microseconds
	offsetFor   linkID
	haveOffset  bool
	measuredAt  time.Time
	measureFail int

	joined  map[string]bool // interfaces we are listening on
	pingCon *net.UDPConn
	note    string
}

func NewAbletonLink() *AbletonLink {
	return &AbletonLink{epoch: time.Now(), peers: map[linkID]*linkPeer{}, joined: map[string]bool{}}
}

// host is our clock in microseconds. Times made with time.Now carry a
// monotonic reading, so this does not jump when the wall clock is set.
func (a *AbletonLink) host(t time.Time) int64 { return t.Sub(a.epoch).Microseconds() }

// LinkState is the session's beat at one moment.
type LinkState struct {
	BPM   float64
	Beat  float64 // running beat position; whole multiples of 4 start a bar
	Peers int
}

// State gives the session's beat at a time, if a session is being heard
// and its clock has been measured.
func (a *AbletonLink) State(at time.Time) (LinkState, bool) {
	if a == nil {
		return LinkState{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.inSession || !a.haveOffset || a.offsetFor != a.session {
		return LinkState{}, false
	}
	return LinkState{BPM: a.tl.bpm(), Beat: a.tl.beats(float64(a.host(at)) + a.offset), Peers: a.livePeers(a.session)}, true
}

// Info describes the listener for the control page.
func (a *AbletonLink) Info() (peers int, bpm float64, ready bool, note string) {
	if a == nil {
		return 0, 0, false, "off"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.inSession {
		if a.note != "" {
			return 0, 0, false, a.note
		}
		return 0, 0, false, "no Link session heard"
	}
	n := a.livePeers(a.session)
	if !a.haveOffset || a.offsetFor != a.session {
		return n, a.tl.bpm(), false, "session heard, measuring its clock"
	}
	return n, a.tl.bpm(), true, ""
}

func (a *AbletonLink) livePeers(s linkID) int {
	n := 0
	for _, p := range a.peers {
		if p.session == s {
			n++
		}
	}
	return n
}

// heard takes in one announcement.
func (a *AbletonLink) heard(m linkAnnouncement, from *net.UDPAddr, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch m.kind {
	case linkByeBye:
		delete(a.peers, m.node)
	case linkAlive, linkResponse:
		if !m.hasTimeline || !m.hasSession {
			return
		}
		p := a.peers[m.node]
		if p == nil {
			p = &linkPeer{}
			a.peers[m.node] = p
		}
		p.session, p.tl, p.seen, p.ttl = m.session, m.tl, now, m.ttl
		if p.ttl <= 0 {
			p.ttl = 5 * time.Second
		}
		// Pings go to the address the announcement came from, never to an
		// address named inside it.
		p.measure = nil
		if m.measurePort > 0 && from != nil {
			p.measure = &net.UDPAddr{IP: from.IP, Port: m.measurePort, Zone: from.Zone}
		}
	default:
		return
	}
	a.choose(now)
}

// choose drops programs that have gone quiet and settles which session to
// follow: the one with the most programs in it, staying put on a tie.
func (a *AbletonLink) choose(now time.Time) {
	count := map[linkID]int{}
	for id, p := range a.peers {
		if now.Sub(p.seen) > p.ttl {
			delete(a.peers, id)
			continue
		}
		count[p.session]++
	}
	was, wasIn := a.session, a.inSession
	var ids []linkID
	for id := range count {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if count[ids[i]] != count[ids[j]] {
			return count[ids[i]] > count[ids[j]]
		}
		if (ids[i] == was) != (ids[j] == was) {
			return wasIn && ids[i] == was
		}
		return bytes.Compare(ids[i][:], ids[j][:]) < 0
	})
	if len(ids) == 0 {
		if wasIn {
			log.Printf("Ableton Link: the session has gone")
		}
		a.inSession = false
		return
	}
	a.session, a.inSession = ids[0], true
	// Within a session the newest timeline is the one with the latest
	// beat origin; the others catch up within a moment.
	first := true
	for _, p := range a.peers {
		if p.session == a.session && (first || p.tl.beatOrigin > a.tl.beatOrigin) {
			a.tl, first = p.tl, false
		}
	}
	if !wasIn || was != a.session {
		log.Printf("Ableton Link: heard a session at %.2f BPM (%d program(s))", a.tl.bpm(), count[a.session])
	}
}

// target picks who to ping, if a measurement is due.
func (a *AbletonLink) target(now time.Time) (*net.UDPAddr, linkID, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.choose(now)
	if !a.inSession {
		return nil, linkID{}, false
	}
	fresh := a.haveOffset && a.offsetFor == a.session
	wait := 30 * time.Second // clocks drift apart slowly: check again now and then
	if !fresh {
		wait = time.Duration(min(a.measureFail, 10)) * 500 * time.Millisecond
	}
	if now.Sub(a.measuredAt) < wait {
		return nil, linkID{}, false
	}
	var best *linkPeer
	for _, p := range a.peers {
		if p.session == a.session && p.measure != nil && (best == nil || p.seen.After(best.seen)) {
			best = p
		}
	}
	if best == nil {
		return nil, linkID{}, false
	}
	return best.measure, a.session, true
}

// measure exchanges pings with one program and returns samples of the
// session clock minus ours. Each answer gives the session clock at the
// moment our ping arrived, which sits midway between our sending it and
// hearing back; the previous answer brackets our send time the same way.
func (a *AbletonLink) measure(to *net.UDPAddr, session linkID) ([]float64, error) {
	const want, tries = 100, 5
	con := a.pingCon
	buf := make([]byte, 512)
	var data []float64
	if _, err := con.WriteToUDP(linkPingMessage(a.host(time.Now()), 0), to); err != nil {
		return nil, err
	}
	for misses := 0; len(data) <= want; {
		_ = con.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, from, err := con.ReadFromUDP(buf)
		got := time.Now()
		if err != nil {
			var ne net.Error
			if !errors.As(err, &ne) || !ne.Timeout() {
				return data, err
			}
			if misses++; misses > tries {
				return data, errors.New("no answer to pings")
			}
			if _, err := con.WriteToUDP(linkPingMessage(a.host(time.Now()), 0), to); err != nil {
				return data, err
			}
			continue
		}
		if !from.IP.Equal(to.IP) || from.Port != to.Port {
			continue
		}
		pong, err := parseLinkPong(buf[:n])
		if err != nil {
			continue
		}
		if pong.session != session {
			return data, errors.New("it has moved to another session")
		}
		misses = 0
		host := a.host(got)
		if _, err := con.WriteToUDP(linkPingMessage(host, pong.ghost), to); err != nil {
			return data, err
		}
		if pong.ghost != 0 && pong.prevHost != 0 {
			data = append(data, float64(pong.ghost)-float64(host+pong.prevHost)/2)
			if pong.prevGhost != 0 {
				data = append(data, float64(pong.ghost+pong.prevGhost)/2-float64(pong.prevHost))
			}
		}
	}
	return data, nil
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	} else {
		return (s[n/2-1] + s[n/2]) / 2
	}
}

// listenOn joins the Link group on every network interface that can carry
// it and is not joined yet. Interfaces come and go (Wi-Fi joining late),
// so this is called again from time to time.
func (a *AbletonLink) listenOn() {
	ifs, err := net.Interfaces()
	if err != nil {
		return
	}
	group := &net.UDPAddr{IP: net.ParseIP(linkGroup), Port: linkPort}
	var lastErr error
	for i := range ifs {
		ifi := ifs[i]
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagLoopback != 0 || !hasIPv4(&ifi) {
			continue
		}
		a.mu.Lock()
		done := a.joined[ifi.Name]
		a.mu.Unlock()
		if done {
			continue
		}
		con, err := net.ListenMulticastUDP("udp4", &ifi, group)
		if err != nil {
			lastErr = err
			continue
		}
		a.mu.Lock()
		a.joined[ifi.Name] = true
		a.mu.Unlock()
		go func() {
			defer func() {
				con.Close()
				a.mu.Lock()
				delete(a.joined, ifi.Name)
				a.mu.Unlock()
			}()
			buf := make([]byte, 1024)
			for {
				n, from, err := con.ReadFromUDP(buf)
				if err != nil {
					return
				}
				if m, err := parseLinkAnnouncement(buf[:n]); err == nil {
					a.heard(m, from, time.Now())
				}
			}
		}()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case len(a.joined) > 0:
		a.note = ""
	case lastErr != nil:
		a.note = fmt.Sprintf("cannot listen for Link (%v)", lastErr)
	default:
		a.note = "no network to listen for Link on"
	}
}

func hasIPv4(ifi *net.Interface) bool {
	addrs, _ := ifi.Addrs()
	for _, ad := range addrs {
		if n, ok := ad.(*net.IPNet); ok && n.IP.To4() != nil {
			return true
		}
	}
	return false
}

// Run listens and keeps the clock offset measured until the program ends.
func (a *AbletonLink) Run() {
	con, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		log.Printf("Ableton Link: cannot open a socket for pings (%v): Link is off", err)
		a.mu.Lock()
		a.note = "cannot open a socket"
		a.mu.Unlock()
		return
	}
	a.pingCon = con
	lastScan := time.Time{}
	for {
		now := time.Now()
		if now.Sub(lastScan) > 5*time.Second {
			lastScan = now
			a.listenOn()
		}
		if to, session, due := a.target(now); due {
			data, err := a.measure(to, session)
			a.mu.Lock()
			a.measuredAt = time.Now()
			if len(data) >= 20 {
				off := median(data)
				if !a.haveOffset || a.offsetFor != session {
					log.Printf("Ableton Link: clock measured against %s (%d samples): following its beat", to.IP, len(data))
				} else if d := off - a.offset; d > 3000 || d < -3000 {
					log.Printf("Ableton Link: the clock offset moved by %.1f ms", d/1000)
				}
				a.offset, a.offsetFor, a.haveOffset, a.measureFail = off, session, true, 0
			} else {
				if a.measureFail++; a.measureFail == 1 || a.measureFail%20 == 0 {
					log.Printf("Ableton Link: could not measure the clock against %s (%v, %d samples)", to.IP, err, len(data))
				}
			}
			a.mu.Unlock()
		}
		time.Sleep(250 * time.Millisecond)
	}
}
