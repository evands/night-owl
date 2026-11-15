package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pion/dtls/v3"
)

// Hue talks to one bridge: the REST API for setup, and the Entertainment
// streaming API (DTLS on UDP 2100) for low-latency colour frames.
type Hue struct {
	ip, user, key string
	client        *http.Client
	id            string // cached application id
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 6 * time.Second,
		Transport: &http.Transport{
			// The bridge presents a self-signed certificate on the LAN.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

func NewHue(ip, user, key string) *Hue {
	return &Hue{ip: ip, user: user, key: key, client: newHTTPClient()}
}

func (h *Hue) do(method, path string, body any) (http.Header, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "https://"+h.ip+path, rd)
	if err != nil {
		return nil, nil, err
	}
	if h.user != "" {
		req.Header.Set("hue-application-key", h.user)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Errors []struct {
				Description string `json:"description"`
			} `json:"errors"`
		}
		he := &httpError{Code: resp.StatusCode, Msg: fmt.Sprintf("bridge returned HTTP %d", resp.StatusCode)}
		if json.Unmarshal(data, &e) == nil && len(e.Errors) > 0 {
			he.Msg = e.Errors[0].Description
		}
		return resp.Header, data, he
	}
	return resp.Header, data, nil
}

// httpError is a reply from the bridge with a failure status.
type httpError struct {
	Code int
	Msg  string
}

func (e *httpError) Error() string { return e.Msg }

// keyRejected reports whether the bridge refused the saved app key, which
// means pairing again rather than a network problem.
func keyRejected(err error) bool {
	var he *httpError
	return errors.As(err, &he) && (he.Code == 401 || he.Code == 403)
}

// bridgeID reads a bridge's stable id; it needs no key.
func bridgeID(ip string) string {
	h := &Hue{ip: ip, client: newHTTPClient()}
	_, data, err := h.do("GET", "/api/0/config", nil)
	if err != nil {
		return ""
	}
	var c struct {
		ID string `json:"bridgeid"`
	}
	_ = json.Unmarshal(data, &c)
	return strings.ToLower(c.ID)
}

// pairBridge asks a bridge for an app key. It succeeds only within ~30 s of
// the bridge's link button being pressed.
func pairBridge(ip string) (user, key string, err error) {
	h := &Hue{ip: ip, client: newHTTPClient()}
	_, data, err := h.do("POST", "/api", map[string]any{"devicetype": "cdjhue#app", "generateclientkey": true})
	if err != nil {
		return "", "", err
	}
	var res []struct {
		Success *struct {
			Username  string `json:"username"`
			ClientKey string `json:"clientkey"`
		} `json:"success"`
		Error *struct {
			Description string `json:"description"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &res); err != nil || len(res) == 0 {
		return "", "", fmt.Errorf("unexpected reply from bridge")
	}
	if res[0].Success == nil {
		if res[0].Error != nil {
			return "", "", errors.New(res[0].Error.Description)
		}
		return "", "", errors.New("pairing refused")
	}
	return res[0].Success.Username, res[0].Success.ClientKey, nil
}

// discoverBridges asks Philips' discovery service which bridges are on this
// network.
func discoverBridges() ([]foundBridge, error) {
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Get("https://discovery.meethue.com/")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var list []foundBridge
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	out := list[:0]
	for _, b := range list {
		if b.IP != "" {
			b.ID = strings.ToLower(b.ID)
			out = append(out, b)
		}
	}
	return out, nil
}

type foundBridge struct {
	ID string `json:"id"`
	IP string `json:"internalipaddress"`
}

// Area is a Hue Entertainment area.
type Area struct {
	ID       string
	Name     string
	Type     string
	Status   string
	Streamer string // application id of whoever is streaming, if anyone
	Channels []Channel
}

type areaJSON struct {
	ID       string `json:"id"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Type     string `json:"configuration_type"`
	Status   string `json:"status"`
	Streamer struct {
		RID string `json:"rid"`
	} `json:"active_streamer"`
	Channels []struct {
		ID       uint8 `json:"channel_id"`
		Position struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
			Z float64 `json:"z"`
		} `json:"position"`
	} `json:"channels"`
}

func (a areaJSON) area() Area {
	out := Area{ID: a.ID, Name: a.Metadata.Name, Type: a.Type, Status: a.Status, Streamer: a.Streamer.RID}
	for _, c := range a.Channels {
		out.Channels = append(out.Channels, Channel{ID: c.ID, X: c.Position.X, Y: c.Position.Y, Z: c.Position.Z})
	}
	return out
}

func (h *Hue) areas(path string) ([]Area, error) {
	_, data, err := h.do("GET", path, nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		Data []areaJSON `json:"data"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, err
	}
	out := make([]Area, 0, len(res.Data))
	for _, a := range res.Data {
		out = append(out, a.area())
	}
	return out, nil
}

func (h *Hue) Areas() ([]Area, error) {
	return h.areas("/clip/v2/resource/entertainment_configuration")
}

// AreaState reports whether the area is being streamed to, and by whom:
// "mine", "other" or "free".
func (h *Hue) AreaState(id string) (string, error) {
	as, err := h.areas("/clip/v2/resource/entertainment_configuration/" + id)
	if err != nil {
		return "", err
	}
	if len(as) == 0 {
		return "", errors.New("entertainment area not found")
	}
	switch {
	case as[0].Status != "active":
		return "free", nil
	case as[0].Streamer == h.appID():
		return "mine", nil
	}
	return "other", nil
}

func (h *Hue) setStreaming(id, action string) error {
	_, _, err := h.do("PUT", "/clip/v2/resource/entertainment_configuration/"+id, map[string]string{"action": action})
	return err
}

// errBusy means another app is streaming to the area.
var errBusy = errors.New("another app is driving these lights")

// StartStream claims the area. With takeover, a stream held by another app
// is stopped first; without it, a busy area is left alone.
func (h *Hue) StartStream(id string, takeover bool) error {
	state, err := h.AreaState(id)
	if err == nil && state == "other" && !takeover {
		return errBusy
	}
	if err == nil && state != "free" {
		// Someone else's stream, or one left over from a previous run of
		// this program: end it so the bridge starts a clean session.
		_ = h.setStreaming(id, "stop")
		time.Sleep(400 * time.Millisecond)
	}
	return h.setStreaming(id, "start")
}

// StopStream releases the area, but only if this app is the one streaming.
func (h *Hue) StopStream(id string) error {
	if state, err := h.AreaState(id); err == nil && state != "mine" {
		return nil
	}
	return h.setStreaming(id, "stop")
}

// appID is the identity the bridge expects in the DTLS handshake.
func (h *Hue) appID() string {
	if h.id != "" {
		return h.id
	}
	hdr, _, err := h.do("GET", "/auth/v1", nil)
	if err == nil {
		if id := hdr.Get("hue-application-id"); id != "" {
			h.id = id
			return id
		}
	}
	return h.user
}

// Dial opens the encrypted streaming connection. Right after a stream is
// started the bridge sometimes ignores a handshake for good, while a fresh
// attempt goes straight through, so each attempt is kept short and retried.
func (h *Hue) Dial(ctx context.Context) (net.Conn, error) {
	var err error
	for attempt := 0; attempt < 4 && ctx.Err() == nil; attempt++ {
		var conn net.Conn
		if conn, err = h.dialOnce(ctx); err == nil {
			return conn, nil
		}
	}
	return nil, err
}

func (h *Hue) dialOnce(ctx context.Context) (net.Conn, error) {
	psk, err := hex.DecodeString(h.key)
	if err != nil {
		return nil, fmt.Errorf("bad client key in config: %w", err)
	}
	cfg := &dtls.Config{
		PSK:             func([]byte) ([]byte, error) { return psk, nil },
		PSKIdentityHint: []byte(h.appID()),
		CipherSuites:    []dtls.CipherSuiteID{dtls.TLS_PSK_WITH_AES_128_GCM_SHA256},
		FlightInterval:  400 * time.Millisecond,
	}
	conn, err := dtls.Dial("udp", &net.UDPAddr{IP: net.ParseIP(h.ip), Port: 2100}, cfg)
	if err != nil {
		return nil, err
	}
	hctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("DTLS handshake with bridge failed: %w", err)
	}
	return conn, nil
}

// frame builds one HueStream v2 message: a colour for every channel.
func frame(seq uint8, areaID string, chans []Channel, cols []RGB) []byte {
	b := make([]byte, 0, 52+7*len(chans))
	b = append(b, "HueStream"...)
	b = append(b, 2, 0, seq, 0, 0, 0 /* RGB */, 0)
	b = append(b, areaID...)
	u16 := func(v float64) (byte, byte) {
		x := uint16(clampF(v, 0, 1)*65535 + 0.5)
		return byte(x >> 8), byte(x)
	}
	for i, c := range chans {
		rh, rl := u16(cols[i].R)
		gh, gl := u16(cols[i].G)
		bh, bl := u16(cols[i].B)
		b = append(b, c.ID, rh, rl, gh, gl, bh, bl)
	}
	return b
}
