package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sort"
	"sync"
	"time"
)

// Pro DJ Link: Pioneer players and mixers broadcast on UDP 50000 (presence)
// and 50001 (a packet on every beat, plus the mixer's on-air channels).
// This listener is passive: it never transmits on the DJ network.

var djMagic = []byte("Qspt1WmJOL")

// BeatPacket is one beat announced by a player.
type BeatPacket struct {
	Player    int
	At        time.Time // local arrival time
	BPM       float64   // effective tempo (track BPM x pitch)
	BeatInBar int       // 1..4
}

// Device is a player or mixer seen on the network.
type Device struct {
	Number   int       `json:"number"`
	Name     string    `json:"name"`
	IP       string    `json:"ip"`
	LastSeen time.Time `json:"-"`
}

type DJLink struct {
	Beats chan BeatPacket

	mu        sync.Mutex
	self      string // our own address when the deck link is on, so we do not list ourselves
	devices   map[int]*Device
	onAir     [4]bool
	onAirSeen time.Time
}

func NewDJLink() *DJLink {
	return &DJLink{Beats: make(chan BeatPacket, 64), devices: map[int]*Device{}}
}

func listenUDP(port int) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: reuseControl}
	return lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", port))
}

// Start opens the sockets and begins listening in the background.
func (d *DJLink) Start() error {
	beatConn, err := listenUDP(50001)
	if err != nil {
		// rekordbox takes this port exclusively, and Windows reports that
		// as a permissions error rather than "address in use".
		return fmt.Errorf("cannot listen for beats on UDP port 50001 (%v).\n"+
			"Another program on this computer is probably holding the port: rekordbox does, "+
			"and so can its helper PSvLinkSysMgr.exe after rekordbox is closed.\n"+
			"Close rekordbox (and end PSvLinkSysMgr.exe if it lingers), then start Night Owl again", err)
	}
	go d.readLoop(beatConn, d.handleBeatPort)
	// Port 50000 only adds device names a little sooner; beats do not need it.
	if annConn, err := listenUDP(50000); err == nil {
		go d.readLoop(annConn, d.handleAnnounce)
	} else {
		log.Printf("UDP port 50000 is held by another program; carrying on without it")
	}
	return nil
}

func (d *DJLink) readLoop(conn net.PacketConn, handle func(pkt []byte, from net.Addr, at time.Time)) {
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFrom(buf)
		at := time.Now()
		if err != nil {
			return
		}
		if n > 0x0b && bytes.HasPrefix(buf[:n], djMagic) {
			handle(buf[:n], from, at)
		}
	}
}

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(bytes.TrimSpace(b))
}

func (d *DJLink) handleAnnounce(pkt []byte, from net.Addr, at time.Time) {
	if pkt[0x0a] != 0x06 || len(pkt) < 0x25 {
		return
	}
	d.noteDevice(int(pkt[0x24]), cstr(pkt[0x0c:0x20]), from, at)
}

func (d *DJLink) noteDevice(num int, name string, from net.Addr, at time.Time) {
	ip := ""
	if ua, ok := from.(*net.UDPAddr); ok {
		ip = ua.IP.String()
	}
	d.mu.Lock()
	if ip != d.self {
		d.devices[num] = &Device{Number: num, Name: name, IP: ip, LastSeen: at}
	}
	d.mu.Unlock()
}

// SetSelf records the address our own announcements come from.
func (d *DJLink) SetSelf(ip string) {
	d.mu.Lock()
	d.self = ip
	d.mu.Unlock()
}

// DeviceIP returns the address of a player seen recently, or "".
func (d *DJLink) DeviceIP(number int) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if dev := d.devices[number]; dev != nil && time.Since(dev.LastSeen) < 8*time.Second {
		return dev.IP
	}
	return ""
}

func (d *DJLink) handleBeatPort(pkt []byte, from net.Addr, at time.Time) {
	// Packets on this port name their sender too, so the gear list still
	// fills in when port 50000 is unavailable.
	if len(pkt) > 0x21 && (pkt[0x0a] == 0x28 || pkt[0x0a] == 0x03) {
		d.noteDevice(int(pkt[0x21]), cstr(pkt[0x0b:0x1f]), from, at)
	}
	switch pkt[0x0a] {
	case 0x28: // beat
		if len(pkt) < 0x5d {
			return
		}
		trackBPM := float64(binary.BigEndian.Uint16(pkt[0x5a:0x5c])) / 100
		pitch := float64(binary.BigEndian.Uint32(pkt[0x54:0x58])) / 0x100000
		bpm := trackBPM * pitch
		bib := int(pkt[0x5c])
		if bpm < 30 || bpm > 400 || bib < 1 || bib > 4 {
			return
		}
		select {
		case d.Beats <- BeatPacket{Player: int(pkt[0x21]), At: at, BPM: bpm, BeatInBar: bib}:
		default: // never block the socket reader
		}
	case 0x03: // mixer: which channels are on air
		if len(pkt) < 0x28 {
			return
		}
		d.mu.Lock()
		for i := 0; i < 4; i++ {
			d.onAir[i] = pkt[0x24+i] != 0
		}
		d.onAirSeen = at
		d.mu.Unlock()
	}
}

// OnAir returns the mixer's channel flags and whether they are fresh.
func (d *DJLink) OnAir(now time.Time) (flags [4]bool, fresh bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.onAir, !d.onAirSeen.IsZero() && now.Sub(d.onAirSeen) < 5*time.Second
}

// Devices lists gear heard from in the last few seconds.
func (d *DJLink) Devices(now time.Time) []Device {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []Device{}
	for _, dev := range d.devices {
		if now.Sub(dev.LastSeen) < 8*time.Second {
			out = append(out, *dev)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}
