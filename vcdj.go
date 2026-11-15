package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// Deck link: optional, off by default.
//
// Everything else in this program only listens. With the deck link on, it
// also announces itself on the DJ network as one more player, which is the
// only way to make the decks send their detailed status: which deck is the
// tempo master, whether each is playing and on air, the beat number within
// the track, and which track is loaded. That in turn allows asking a deck
// for the track's phrase analysis. The announcement is the same keep-alive
// packet Beat Link Trigger and similar tools have sent at gigs for years.

// DeckState is the latest status packet from one player.
type DeckState struct {
	Player     int
	At         time.Time
	Playing    bool
	Master     bool
	Synced     bool
	OnAir      bool
	BeatNumber int64 // beat within the track, 1-based; 0 when unknown
	BeatInBar  int
	TrackID    uint32
	Source     byte // player whose media the track was loaded from
	Slot       byte // 2 = SD, 3 = USB
	TrackType  byte // 1 = rekordbox-analysed
	Flags      byte // the raw status flag byte
	PlayState  byte // the deck's own play-state code (3 playing, 4 looping, 5 paused, 6 cued...)
}

// TrackKey identifies a loaded track for the phrase cache.
type TrackKey struct {
	Source, Slot byte
	ID           uint32
}

func (s DeckState) trackKey() (TrackKey, bool) {
	if s.TrackType != 1 || s.TrackID == 0 || s.Source == 0 {
		return TrackKey{}, false
	}
	return TrackKey{s.Source, s.Slot, s.TrackID}, true
}

// parseStatus reads a CDJ status packet (UDP 50002, type 0x0a).
func parseStatus(pkt []byte, at time.Time) (DeckState, bool) {
	if len(pkt) < 0xa7 || pkt[0x0a] != 0x0a {
		return DeckState{}, false
	}
	f := pkt[0x89]
	st := DeckState{
		Player: int(pkt[0x21]), At: at,
		Playing: f&0x40 != 0, Master: f&0x20 != 0, Synced: f&0x10 != 0, OnAir: f&0x08 != 0,
		BeatInBar: int(pkt[0xa6]),
		TrackID:   binary.BigEndian.Uint32(pkt[0x2c:0x30]),
		Source:    pkt[0x28], Slot: pkt[0x29], TrackType: pkt[0x2a],
		Flags: f, PlayState: pkt[0x7b],
	}
	if bn := binary.BigEndian.Uint32(pkt[0xa0:0xa4]); bn != 0xffffffff {
		st.BeatNumber = int64(bn)
	}
	return st, true
}

// keepAlive builds the presence packet a player broadcasts every 1.5 s.
func keepAlive(number int, mac net.HardwareAddr, ip net.IP) []byte {
	p := make([]byte, 0x36)
	copy(p, djMagic)
	p[0x0a] = 0x06
	copy(p[0x0c:0x20], "Night Owl")
	p[0x20], p[0x21] = 0x01, 0x02
	p[0x23] = 0x36 // packet length
	p[0x24] = byte(number)
	p[0x25] = 0x01
	copy(p[0x26:0x2c], mac)
	copy(p[0x2c:0x30], ip.To4())
	p[0x30] = 0x01
	p[0x34] = 0x01 // device type: player
	return p
}

// VCDJ is the virtual player.
type VCDJ struct {
	dj       *DJLink
	phrases  *PhraseStore
	onStatus func(DeckState)

	forceNumber int // 0 = choose automatically

	mu      sync.Mutex
	asked   map[string]int // what the decks send us besides status, for diagnosis
	active  bool
	number  int
	note    string
	states  map[int]DeckState
	greeted map[int]bool
}

func NewVCDJ(dj *DJLink, phrases *PhraseStore, onStatus func(DeckState)) *VCDJ {
	return &VCDJ{dj: dj, phrases: phrases, onStatus: onStatus, note: "off", states: map[int]DeckState{}, greeted: map[int]bool{}}
}

// Info is what the control page shows about the link.
func (v *VCDJ) Info() (active bool, number int, note string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.active, v.number, v.note
}

// State returns a player's status if it is recent enough to trust.
func (v *VCDJ) State(player int, now time.Time) (DeckState, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	st, ok := v.states[player]
	return st, ok && v.active && now.Sub(st.At) < 1500*time.Millisecond
}

func (v *VCDJ) setNote(active bool, number int, note string) {
	v.mu.Lock()
	changed := v.note != note
	v.active, v.number, v.note = active, number, note
	v.mu.Unlock()
	if changed {
		log.Printf("deck link: %s", note)
	}
}

// localAddrFor finds this machine's address on the same network as ip.
func localAddrFor(ip net.IP) (net.IP, net.IP, net.HardwareAddr) {
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || len(ifc.HardwareAddr) != 6 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || !ipn.Contains(ip) {
				continue
			}
			local := ipn.IP.To4()
			bcast := make(net.IP, 4)
			for i := range bcast {
				bcast[i] = local[i] | ^ipn.Mask[len(ipn.Mask)-4+i]
			}
			return local, bcast, ifc.HardwareAddr
		}
	}
	return nil, nil, nil
}

// Run switches the link on and off as the setting changes. wanted is polled
// so the control page toggle takes effect within a second.
func (v *VCDJ) Run(wanted func() (bool, string)) {
	statusConn, err := listenUDP(50002)
	if err != nil {
		v.setNote(false, 0, "unavailable: UDP port 50002 is held by another program (rekordbox?)")
		return
	}
	go v.dj.readLoop(statusConn, func(pkt []byte, from net.Addr, at time.Time) {
		st, ok := parseStatus(pkt, at)
		if !ok {
			// Anything else a deck sends to a fellow player (media queries
			// and the like) goes unanswered; count it so we can see it.
			if len(pkt) > 0x21 {
				v.mu.Lock()
				if v.asked == nil {
					v.asked = map[string]int{}
				}
				v.asked[fmt.Sprintf("packet type 0x%02x (%d bytes) from device %d", pkt[0x0a], len(pkt), pkt[0x21])]++
				v.mu.Unlock()
			}
			return
		}
		v.mu.Lock()
		prev, had := v.states[st.Player]
		v.states[st.Player] = st
		// Keep a record of SYNC and tempo-master changes on each deck, so
		// "did sync drop, and when?" can be answered from the log.
		if had && at.Sub(prev.At) < 5*time.Second {
			if prev.Synced != st.Synced {
				log.Printf("deck link: deck %d SYNC is now %s", st.Player, map[bool]string{true: "ON", false: "OFF"}[st.Synced])
			}
			if prev.Playing != st.Playing || prev.PlayState != st.PlayState {
				log.Printf("deck link: deck %d play state %d -> %d (playing flag %v, flags 0x%02x, beat %d)", st.Player, prev.PlayState, st.PlayState, st.Playing, st.Flags, st.BeatNumber)
			}
			if prev.Master != st.Master {
				log.Printf("deck link: deck %d %s tempo master", st.Player, map[bool]string{true: "became", false: "is no longer"}[st.Master])
			}
		}
		first := !v.greeted[st.Player]
		v.greeted[st.Player] = true
		number := v.number
		v.mu.Unlock()
		if first {
			log.Printf("deck link: deck %d is sending status (playing %v, master %v, sync %v, on air %v, beat %d, track %d; flags 0x%02x, play state %d)",
				st.Player, st.Playing, st.Master, st.Synced, st.OnAir, st.BeatNumber, st.TrackID, st.Flags, st.PlayState)
		}
		if v.onStatus != nil {
			v.onStatus(st)
		}
		if key, ok := st.trackKey(); ok && number >= 1 && number <= 4 {
			if ip := v.dj.DeviceIP(int(st.Source)); ip != "" {
				v.phrases.Request(key, ip, number)
			}
		}
	})

	var out *net.UDPConn
	var pkt []byte
	stop := func(note string) {
		if out != nil {
			out.Close()
			out = nil
		}
		v.dj.SetSelf("")
		v.setNote(false, 0, note)
	}
	// Decks may also try to open their database connection to us. Nothing
	// listens there normally, and a firewall that silently drops the attempt
	// would leave a deck waiting on it. Accept, say "no database", and log.
	if ln, err := net.Listen("tcp4", ":12523"); err == nil {
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				v.mu.Lock()
				if v.asked == nil {
					v.asked = map[string]int{}
				}
				v.asked["database port lookup (TCP 12523) from "+c.RemoteAddr().(*net.TCPAddr).IP.String()]++
				v.mu.Unlock()
				go func() {
					defer c.Close()
					_ = c.SetDeadline(time.Now().Add(time.Second))
					buf := make([]byte, 32)
					_, _ = c.Read(buf)
					_, _ = c.Write([]byte{0, 0})
				}()
			}
		}()
	}
	tick := time.NewTicker(500 * time.Millisecond)
	lastSent := time.Time{}
	lastReport := time.Now()
	freeSince := time.Time{}
	for now := range tick.C {
		if now.Sub(lastReport) > 10*time.Second {
			lastReport = now
			v.mu.Lock()
			for what, n := range v.asked {
				log.Printf("deck link: received %d x %s", n, what)
			}
			v.asked = nil
			v.mu.Unlock()
		}
		if on, why := wanted(); !on {
			if _, _, note := v.Info(); out != nil || note != why {
				stop(why)
			}
			continue
		}
		devices := v.dj.Devices(now)
		if out == nil {
			// Join: need a deck to find the right network, and a free number.
			var deck *Device
			used := map[int]bool{}
			for i := range devices {
				used[devices[i].Number] = true
				if devices[i].Number >= 1 && devices[i].Number <= 6 && deck == nil {
					deck = &devices[i]
				}
			}
			if deck == nil {
				v.setNote(false, 0, "waiting for a deck to appear")
				continue
			}
			local, bcast, mac := localAddrFor(net.ParseIP(deck.IP))
			if local == nil {
				v.setNote(false, 0, fmt.Sprintf("this computer has no address on the decks' network (%s)", deck.IP))
				continue
			}
			number := 0
			if v.forceNumber > 0 && !used[v.forceNumber] {
				number = v.forceNumber
			}
			for _, n := range []int{4, 3, 2, 1} { // 1-4 are the numbers decks answer track queries from
				if number == 0 && v.forceNumber == 0 && !used[n] {
					number = n
				}
			}
			note := ""
			if number == 0 {
				number = 7
				for used[number] {
					number++
				}
				note = " (all four player numbers are taken, so track structure is unavailable)"
			}
			conn, err := net.DialUDP("udp4", &net.UDPAddr{IP: local}, &net.UDPAddr{IP: bcast, Port: 50000})
			if err != nil {
				v.setNote(false, 0, "could not open a socket on the decks' network: "+err.Error())
				continue
			}
			out, pkt = conn, keepAlive(number, mac, local)
			v.dj.SetSelf(local.String())
			v.setNote(true, number, fmt.Sprintf("on the DJ network as player %d%s", number, note))
			lastSent = time.Time{}
		}
		// Joined above 4 because 1-4 were all taken (so no track structure)?
		// Move down once one of them has been free for a while.
		_, number, _ := v.Info()
		if number > 4 && v.forceNumber == 0 {
			taken := map[int]bool{}
			for _, d := range devices {
				taken[d.Number] = true
			}
			if !(taken[1] && taken[2] && taken[3] && taken[4]) {
				if freeSince.IsZero() {
					freeSince = now
				} else if now.Sub(freeSince) > 12*time.Second {
					freeSince = time.Time{}
					stop("rejoining with a player number from 1 to 4")
					continue
				}
			} else {
				freeSince = time.Time{}
			}
		}
		// Yield at once if a real device turns up with our number.
		for _, d := range devices {
			if d.Number == number {
				stop(fmt.Sprintf("stopped: a real device (%s) is using player number %d", d.Name, number))
				time.Sleep(5 * time.Second)
				break
			}
		}
		if out != nil && now.Sub(lastSent) >= 1500*time.Millisecond {
			if _, err := out.Write(pkt); err != nil {
				stop("stopped: " + err.Error())
				continue
			}
			lastSent = now
		}
	}
}
