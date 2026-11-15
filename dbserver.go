package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// Track structure: rekordbox analyses each track into phrases (intro, verse,
// chorus, and so on) and exports them as a "PSSI" tag. A deck serves that
// tag from its database server to other players on the link.

// Phrase is one section of a track.
type Phrase struct {
	Beat   int // first beat of the phrase, 1-based within the track
	Label  string
	Energy float64 // 0..1, how intense this kind of section is
}

// Structure is a track's phrase analysis.
type Structure struct {
	Mood    string // high | mid | low
	EndBeat int
	Phrases []Phrase
}

// At returns the phrase containing a beat, its index, and beats into it.
func (st *Structure) At(beat int64) (Phrase, int, int64, bool) {
	if st == nil || len(st.Phrases) == 0 || beat < int64(st.Phrases[0].Beat) {
		return Phrase{}, 0, 0, false
	}
	i := 0
	for i+1 < len(st.Phrases) && int64(st.Phrases[i+1].Beat) <= beat {
		i++
	}
	return st.Phrases[i], i, beat - int64(st.Phrases[i].Beat), true
}

var pssiMask = [19]byte{0xCB, 0xE1, 0xEE, 0xFA, 0xE5, 0xEE, 0xAD, 0xEE, 0xE9, 0xD2, 0xE9, 0xEB, 0xE1, 0xE9, 0xF3, 0xE8, 0xE9, 0xF4, 0xE1}

func phraseInfo(mood, kind int, k1, k2, k3 byte) (string, float64) {
	switch mood {
	case 1: // high
		switch kind {
		case 1:
			return "Intro", 0.35
		case 2:
			return "Up", 0.75
		case 3:
			return "Down", 0.35
		case 5:
			return "Chorus", 1.0
		case 6:
			return "Outro", 0.35
		}
	case 2: // mid
		switch {
		case kind == 1:
			return "Intro", 0.35
		case kind >= 2 && kind <= 7:
			return fmt.Sprintf("Verse %d", kind-1), 0.45 + 0.05*float64(kind-1)
		case kind == 8:
			return "Bridge", 0.45
		case kind == 9:
			return "Chorus", 1.0
		case kind == 10:
			return "Outro", 0.35
		}
	case 3: // low
		switch {
		case kind == 1:
			return "Intro", 0.3
		case kind >= 2 && kind <= 4:
			return "Verse 1", 0.45
		case kind >= 5 && kind <= 7:
			return "Verse 2", 0.55
		case kind == 8:
			return "Bridge", 0.4
		case kind == 9:
			return "Chorus", 0.8
		case kind == 10:
			return "Outro", 0.3
		}
	}
	return fmt.Sprintf("Section %d", kind), 0.6
}

// parsePSSI decodes a song structure tag, undoing the masking rekordbox 6
// applies to everything after the entry count.
func parsePSSI(tag []byte) (*Structure, error) {
	// Real decks put a 4-byte length in front of the tag (seen on
	// CDJ-2000NXS2: "48 02 00 00 PSSI..." for a 588-byte reply).
	if i := bytes.Index(tag[:min(len(tag), 16)], []byte("PSSI")); i > 0 {
		tag = tag[i:]
	}
	if len(tag) < 0x20 || string(tag[:4]) != "PSSI" {
		return nil, fmt.Errorf("not a PSSI tag (%d bytes, starts % x)", len(tag), tag[:min(len(tag), 8)])
	}
	b := append([]byte(nil), tag...)
	header := int(binary.BigEndian.Uint32(b[4:8]))
	entrySize := int(binary.BigEndian.Uint32(b[12:16]))
	n := int(binary.BigEndian.Uint16(b[16:18]))
	if mood := binary.BigEndian.Uint16(b[18:20]); mood < 1 || mood > 3 {
		for i := 18; i < len(b); i++ {
			b[i] ^= pssiMask[(i-18)%len(pssiMask)] + byte(n)
		}
	}
	mood := int(binary.BigEndian.Uint16(b[18:20]))
	if mood < 1 || mood > 3 {
		return nil, fmt.Errorf("PSSI mood %d is not valid even after unmasking", mood)
	}
	if header < 0x20 || entrySize < 0x18 || header+n*entrySize > len(b) {
		return nil, fmt.Errorf("PSSI layout does not fit: header %d, %d entries of %d in %d bytes", header, n, entrySize, len(b))
	}
	st := &Structure{Mood: [...]string{"", "high", "mid", "low"}[mood], EndBeat: int(binary.BigEndian.Uint16(b[0x1a:0x1c]))}
	last := 0
	for i := 0; i < n; i++ {
		e := b[header+i*entrySize:]
		beat := int(binary.BigEndian.Uint16(e[2:4]))
		if beat < last {
			return nil, fmt.Errorf("PSSI phrase %d starts at beat %d, before the previous one", i+1, beat)
		}
		last = beat
		label, energy := phraseInfo(mood, int(binary.BigEndian.Uint16(e[4:6])), e[7], e[9], e[0x13])
		st.Phrases = append(st.Phrases, Phrase{Beat: beat, Label: label, Energy: energy})
	}
	return st, nil
}

// ---- dbserver wire format: every value is a tagged field.

func num1(v byte) []byte   { return []byte{0x0f, v} }
func num2(v uint16) []byte { return []byte{0x10, byte(v >> 8), byte(v)} }
func num4(v uint32) []byte { return []byte{0x11, byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)} }

// dbMessage builds a message whose arguments are all 4-byte numbers.
func dbMessage(tx uint32, kind uint16, args ...uint32) []byte {
	m := append(num4(0x872349ae), num4(tx)...)
	m = append(m, num2(kind)...)
	m = append(m, num1(byte(len(args)))...)
	tags := make([]byte, 12)
	for i := range args {
		tags[i] = 0x06
	}
	m = append(m, 0x14, 0, 0, 0, 12)
	m = append(m, tags...)
	for _, a := range args {
		m = append(m, num4(a)...)
	}
	return m
}

// readField reads one tagged field: a number, or a blob/string as bytes.
func readField(r *bufio.Reader) (num uint32, blob []byte, err error) {
	t, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var buf [4]byte
	switch t {
	case 0x0f:
		_, err = io.ReadFull(r, buf[:1])
		return uint32(buf[0]), nil, err
	case 0x10:
		_, err = io.ReadFull(r, buf[:2])
		return uint32(binary.BigEndian.Uint16(buf[:2])), nil, err
	case 0x11:
		_, err = io.ReadFull(r, buf[:4])
		return binary.BigEndian.Uint32(buf[:4]), nil, err
	case 0x14, 0x26:
		if _, err = io.ReadFull(r, buf[:4]); err != nil {
			return 0, nil, err
		}
		n := int(binary.BigEndian.Uint32(buf[:4]))
		if t == 0x26 {
			n *= 2 // strings count UTF-16 characters
		}
		if n > 1<<20 {
			return 0, nil, fmt.Errorf("field of %d bytes is implausibly large", n)
		}
		blob = make([]byte, n)
		_, err = io.ReadFull(r, blob)
		return 0, blob, err
	}
	return 0, nil, fmt.Errorf("unknown field type 0x%02x", t)
}

// readHeader reads a message up to its arguments.
func readHeader(r *bufio.Reader) (kind uint16, argc int, err error) {
	magic, _, err := readField(r)
	if err != nil {
		return 0, 0, err
	}
	if magic != 0x872349ae {
		return 0, 0, fmt.Errorf("bad message start 0x%08x", magic)
	}
	if _, _, err = readField(r); err != nil { // transaction id
		return 0, 0, err
	}
	k, _, err := readField(r)
	if err != nil {
		return 0, 0, err
	}
	n, _, err := readField(r)
	if err != nil {
		return 0, 0, err
	}
	if _, _, err = readField(r); err != nil { // argument tags
		return 0, 0, err
	}
	return uint16(k), int(n), nil
}

var errNoPhrases = errors.New("the track has no phrase analysis")

// fetchPSSI asks the player holding a track's media for its PSSI tag.
func fetchPSSI(playerIP string, ourNumber int, key TrackKey) ([]byte, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	// 1. Ask where the database server is.
	c, err := d.Dial("tcp4", net.JoinHostPort(playerIP, "12523"))
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c.Write(append([]byte{0, 0, 0, 0x0f}, "RemoteDBServer\x00"...))
	var pb [2]byte
	_, err = io.ReadFull(c, pb[:])
	c.Close()
	if err != nil {
		return nil, fmt.Errorf("port query: %w", err)
	}
	port := int(binary.BigEndian.Uint16(pb[:]))
	if port == 0 {
		return nil, errors.New("the deck's database server is not available")
	}

	// 2. Connect, greet, introduce ourselves.
	c, err = d.Dial("tcp4", net.JoinHostPort(playerIP, fmt.Sprint(port)))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	if _, err = c.Write(num4(1)); err != nil {
		return nil, err
	}
	if v, _, err := readField(r); err != nil || v != 1 {
		return nil, fmt.Errorf("greeting: got %d, %v", v, err)
	}
	if _, err = c.Write(dbMessage(0xfffffffe, 0x0000, uint32(ourNumber))); err != nil {
		return nil, err
	}
	kind, argc, err := readHeader(r)
	if err != nil {
		return nil, fmt.Errorf("setup reply: %w", err)
	}
	for i := 0; i < argc; i++ {
		if _, _, err = readField(r); err != nil {
			return nil, fmt.Errorf("setup reply: %w", err)
		}
	}
	if kind != 0x4000 {
		return nil, fmt.Errorf("setup refused (reply type 0x%04x)", kind)
	}

	// 3. Request the PSSI tag from the track's extended analysis file.
	dmst := uint32(ourNumber)<<24 | 1<<16 | uint32(key.Slot)<<8 | 1
	if _, err = c.Write(dbMessage(1, 0x2c04, dmst, key.ID, 0x49535350 /* "PSSI" reversed */, 0x00545845 /* "EXT" reversed */)); err != nil {
		return nil, err
	}
	kind, argc, err = readHeader(r)
	if err != nil {
		return nil, fmt.Errorf("tag reply: %w", err)
	}
	if kind != 0x4f02 || argc < 3 {
		return nil, fmt.Errorf("unexpected tag reply type 0x%04x with %d arguments", kind, argc)
	}
	var length uint32
	for i := 0; i < 3; i++ {
		if length, _, err = readField(r); err != nil {
			return nil, fmt.Errorf("tag reply: %w", err)
		}
	}
	var tag []byte
	if length > 0 { // with no data the blob argument is simply absent
		if _, tag, err = readField(r); err != nil {
			return nil, fmt.Errorf("tag data: %w", err)
		}
	}
	_, _ = c.Write(dbMessage(0xfffffffe, 0x0100)) // goodbye
	if len(tag) == 0 {
		return nil, errNoPhrases
	}
	return tag, nil
}

// PhraseStore fetches and caches track structures, one request per track.
type PhraseStore struct {
	mu      sync.Mutex
	entries map[TrackKey]*phraseEntry
}

type phraseEntry struct {
	st    *Structure
	done  bool
	tried time.Time
	tries int
	err   error
}

func NewPhraseStore() *PhraseStore { return &PhraseStore{entries: map[TrackKey]*phraseEntry{}} }

func (p *PhraseStore) Lookup(key TrackKey) *Structure {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.entries[key]; e != nil {
		return e.st
	}
	return nil
}

// Request starts a fetch unless this track was already fetched or tried
// recently. It never blocks.
func (p *PhraseStore) Request(key TrackKey, playerIP string, ourNumber int) {
	p.mu.Lock()
	e := p.entries[key]
	tries := 0
	if e != nil {
		// One answer per track is enough. A failure is retried twice, minutes
		// apart, in case the deck was busy; never while it is mid-request.
		if e.st != nil || !e.done || errors.Is(e.err, errNoPhrases) || e.tries >= 3 || time.Since(e.tried) < 2*time.Minute {
			p.mu.Unlock()
			return
		}
		tries = e.tries
	}
	e = &phraseEntry{tried: time.Now(), tries: tries + 1}
	p.entries[key] = e
	p.mu.Unlock()

	go func() {
		tag, err := fetchPSSI(playerIP, ourNumber, key)
		var st *Structure
		if err == nil {
			st, err = parsePSSI(tag)
		}
		p.mu.Lock()
		e.st, e.err, e.done = st, err, true
		p.mu.Unlock()
		if err != nil {
			log.Printf("track %d (on the media in player %d): no phrase data: %v", key.ID, key.Source, err)
			return
		}
		log.Printf("track %d (on the media in player %d): %d phrases, %s mood, %d beats", key.ID, key.Source, len(st.Phrases), st.Mood, st.EndBeat)
	}()
}
