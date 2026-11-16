// cdjhue syncs Philips Hue lights to Pioneer CDJs.
//
// It listens to the beat packets the decks broadcast over their Pro DJ Link
// network, keeps a steady beat grid from them, and streams colours to a Hue
// Entertainment area slightly ahead of each beat so the light lands on it.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// appName is how the program introduces itself. The command, its settings
// folder and its service keep the short name cdjhue.
const appName = "Night Owl by Groove Formula"

// besideExe is where settings used to live: next to the program.
func besideExe() string {
	exe, err := os.Executable()
	if err != nil {
		return "cdjhue.json"
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Join(filepath.Dir(exe), "cdjhue.json")
}

// defaultConfigPath is a per-user location (%APPDATA%\cdjhue on Windows,
// ~/Library/Application Support/cdjhue on a Mac), so the pairing and
// settings survive the program being moved or downloaded again.
func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return besideExe()
	}
	return filepath.Join(dir, "cdjhue", "cdjhue.json")
}

// listenUI opens the control page port, moving up a few ports if the first
// choice is taken by something else.
func listenUI(addr string) (net.Listener, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	var last error
	for i := 0; i < 10; i++ {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port+i)))
		if err == nil {
			return ln, nil
		}
		last = err
	}
	return nil, last
}

func main() {
	cfgPath := flag.String("config", defaultConfigPath(), "settings file")
	uiAddr := flag.String("ui", ":8600", "address for the control page")
	bridge := flag.String("bridge", "", "Hue bridge IP address (skips discovery during setup)")
	open := flag.Bool("open", true, "open the control page in a browser on start")
	wait := flag.Bool("wait", false, "if another app is driving the lights at start, wait for it instead of taking over")
	_ = flag.Bool("experimental-deck-link", false, "no longer needed: the deck link is switched on and off from the control page")
	linkNumber := flag.Int("link-number", 0, "player number for the deck link (0 = highest free of 4..1; 7 or more = status only, no track structure)")
	retry := flag.Bool("retry", false, "keep trying if the Hue bridge cannot be reached at start (used by auto-start, when the network may not be up yet)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, appName)
		fmt.Fprintln(os.Stderr, "Usage: cdjhue [options] [run|setup|listen]")
		fmt.Fprintln(os.Stderr, "  run     sync the lights (default; runs setup first if needed)")
		fmt.Fprintln(os.Stderr, "  setup   pair with a Hue bridge and choose an Entertainment area")
		fmt.Fprintln(os.Stderr, "  listen  print what the decks are sending, without touching the lights")
		fmt.Fprintln(os.Stderr, "  autostart on|off   start with Windows when you sign in, or stop doing so")
		flag.PrintDefaults()
	}
	flag.Parse()
	log.SetFlags(log.Ltime)
	// Keep a copy of the log beside the settings, for troubleshooting.
	_ = os.MkdirAll(filepath.Dir(*cfgPath), 0o700)
	if f, err := os.OpenFile(strings.TrimSuffix(*cfgPath, filepath.Ext(*cfgPath))+".log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, f))
	}

	store, err := loadStore(*cfgPath, besideExe())
	if err != nil {
		log.Fatalf("cannot read %s: %v", *cfgPath, err)
	}

	switch flag.Arg(0) {
	case "listen":
		err = listen()
	case "autostart":
		err = autostart(flag.Arg(1))
	case "setup":
		err = setup(store, *bridge)
	case "", "run":
		err = run(store, *bridge, *uiAddr, *open, !*wait, *retry, *linkNumber)
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		log.Print(err)
		if runtime.GOOS == "windows" {
			// A double-clicked console window would vanish with the message.
			fmt.Print("Press Enter to close...")
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		}
		os.Exit(1)
	}
}

// warnIfSilent explains the usual causes when no DJ gear is heard.
func warnIfSilent(dj *DJLink, ab *AbletonLink) {
	time.Sleep(10 * time.Second)
	if len(dj.Devices(time.Now())) > 0 || dj.StartErr() != "" {
		return
	}
	if n, _, _, _ := ab.Info(); n > 0 {
		return // beats are coming from Ableton Link instead
	}
	log.Printf("No DJ gear heard after 10 s. Check that:")
	log.Printf("  1. the decks' network switch is cabled to the router;")
	log.Printf("  2. the firewall allows this program on private networks;")
	log.Printf("  3. the decks are not on 169.254.x.x addresses (see \"If the decks do not show up\" in README.md).")
}

// listen prints gear and beats: a quick check that the decks are reachable.
func listen() error {
	dj := NewDJLink()
	dj.StartOrRetry()
	ab := NewAbletonLink()
	go ab.Run()
	go warnIfSilent(dj, ab)
	fmt.Println("Listening for Pioneer DJ gear and Ableton Link (Ctrl-C to stop)...")
	seen := map[int]bool{}
	tick := time.NewTicker(time.Second)
	fast := time.NewTicker(5 * time.Millisecond)
	lastLink, linkPeers := int64(math.MinInt64), -1
	for {
		select {
		case b := <-dj.Beats:
			fmt.Printf("%s  deck %d  beat %d  %.2f BPM\n", b.At.Format("15:04:05.000"), b.Player, b.BeatInBar, b.BPM)
		case now := <-fast.C:
			// Print each Link beat as it passes.
			if st, ok := ab.State(now); ok {
				if n := int64(math.Floor(st.Beat)); n != lastLink {
					lastLink = n
					fmt.Printf("%s  Ableton Link  beat %d  %.2f BPM\n", now.Format("15:04:05.000"), mod(n, 4)+1, st.BPM)
				}
			}
		case now := <-tick.C:
			for _, d := range dj.Devices(now) {
				if !seen[d.Number] {
					seen[d.Number] = true
					fmt.Printf("found %s (#%d) at %s\n", d.Name, d.Number, d.IP)
				}
			}
			if n, bpm, ready, note := ab.Info(); n != linkPeers {
				linkPeers = n
				switch {
				case n == 0:
					fmt.Printf("Ableton Link: %s\n", note)
				case ready:
					fmt.Printf("Ableton Link: %d program(s) at %.2f BPM\n", n, bpm)
				default:
					fmt.Printf("Ableton Link: %d program(s) at %.2f BPM (%s)\n", n, bpm, note)
				}
			}
		}
	}
}

// pair gets an app key from a bridge. With several bridges on the network,
// the one whose button is pressed is the one that answers.
func pair(store *Store, bridgeIP string) error {
	ips := []string{bridgeIP}
	if bridgeIP == "" {
		found, err := discoverBridges()
		if err != nil || len(found) == 0 {
			return fmt.Errorf("no Hue bridge found automatically; run again with -bridge <ip address> (%v)", err)
		}
		ips = ips[:0]
		for _, b := range found {
			ips = append(ips, b.IP)
		}
	}
	fmt.Printf("Found %d Hue bridge(s). Press the round button on the bridge for this room now...\n", len(ips))
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		for _, ip := range ips {
			user, key, err := pairBridge(ip)
			if err != nil {
				continue
			}
			id := bridgeID(ip)
			fmt.Printf("Paired with the bridge at %s.\n", ip)
			return store.Update(func(c *Config) {
				if id == "" || id != c.BridgeID {
					c.AreaID, c.AreaName = "", "" // a different bridge: its areas differ
				}
				c.BridgeIP, c.BridgeID, c.Username, c.ClientKey = ip, id, user, key
			})
		}
		time.Sleep(time.Second)
	}
	return errors.New("timed out waiting for the bridge button")
}

// connect returns a working bridge connection from the saved pairing. It
// pairs only when there is no saved key or the bridge rejects it, and
// re-finds the bridge by its id if its address has changed.
func connect(store *Store, bridgeIP string) (*Hue, []Area, error) {
	cfg := store.Config()
	if cfg.BridgeIP == "" || cfg.Username == "" || cfg.ClientKey == "" {
		if err := pair(store, bridgeIP); err != nil {
			return nil, nil, err
		}
		cfg = store.Config()
	}
	for attempt := 0; ; attempt++ {
		hue := NewHue(cfg.BridgeIP, cfg.Username, cfg.ClientKey)
		areas, err := hue.Areas()
		if err == nil {
			if cfg.BridgeID == "" {
				if id := bridgeID(cfg.BridgeIP); id != "" {
					_ = store.Update(func(c *Config) { c.BridgeID = id })
				}
			}
			return hue, areas, nil
		}
		if attempt > 0 {
			return nil, nil, fmt.Errorf("cannot use the Hue bridge at %s: %w", cfg.BridgeIP, err)
		}
		if keyRejected(err) {
			log.Printf("the bridge no longer accepts the saved key: pairing again")
			if err := pair(store, cfg.BridgeIP); err != nil {
				return nil, nil, err
			}
		} else {
			ip := ""
			if found, derr := discoverBridges(); derr == nil && cfg.BridgeID != "" {
				for _, b := range found {
					if b.ID == cfg.BridgeID {
						ip = b.IP
					}
				}
			}
			if ip == "" || ip == cfg.BridgeIP {
				return nil, nil, fmt.Errorf("cannot reach the Hue bridge at %s: %w", cfg.BridgeIP, err)
			}
			log.Printf("the Hue bridge has moved from %s to %s", cfg.BridgeIP, ip)
			if err := store.Update(func(c *Config) { c.BridgeIP = ip }); err != nil {
				return nil, nil, err
			}
		}
		cfg = store.Config()
	}
}

// chooseArea picks the Entertainment area, asking when there are several.
func chooseArea(store *Store, areas []Area) (*Area, error) {
	if len(areas) == 0 {
		return nil, errors.New("this bridge has no Entertainment area: create one in the Hue app (Settings > Entertainment areas), then start Night Owl again")
	}
	pick := 0
	if len(areas) > 1 {
		for i, a := range areas {
			if a.Type == "music" {
				pick = i
				break
			}
		}
		fmt.Println("Entertainment areas on this bridge:")
		for i, a := range areas {
			fmt.Printf("  %d. %s (%s, %d channels)\n", i+1, a.Name, a.Type, len(a.Channels))
		}
		fmt.Printf("Which one? [%d]: ", pick+1)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if n, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && n >= 1 && n <= len(areas) {
			pick = n - 1
		}
	}
	a := &areas[pick]
	fmt.Printf("Using Entertainment area %q.\n", a.Name)
	return a, store.Update(func(c *Config) { c.AreaID, c.AreaName = a.ID, a.Name })
}

// setup is the explicit "start over": pair again and choose the area again.
func setup(store *Store, bridgeIP string) error {
	if err := pair(store, bridgeIP); err != nil {
		return err
	}
	_, areas, err := connect(store, bridgeIP)
	if err != nil {
		return err
	}
	_, err = chooseArea(store, areas)
	return err
}

// alreadyRunning looks for another copy of this program by its control
// page. Two copies share one bridge key, so the bridge cannot tell them
// apart and each keeps cutting off the other's stream.
func alreadyRunning(uiAddr string) string {
	_, portStr, err := net.SplitHostPort(uiAddr)
	if err != nil {
		return ""
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return ""
	}
	client := &http.Client{Timeout: 400 * time.Millisecond}
	for i := 0; i < 10; i++ {
		url := fmt.Sprintf("http://127.0.0.1:%d", port+i)
		resp, err := client.Get(url + "/api/state")
		if err != nil {
			continue
		}
		var probe struct {
			Status   *json.RawMessage `json:"status"`
			Settings *json.RawMessage `json:"settings"`
		}
		ok := json.NewDecoder(resp.Body).Decode(&probe) == nil && probe.Status != nil && probe.Settings != nil
		resp.Body.Close()
		if ok {
			return url
		}
	}
	return ""
}

// autostart adds or removes a small launcher in the Windows Startup folder.
// The launcher starts the program minimised, without opening a browser, and
// has it wait for the network.
func autostart(arg string) error {
	if runtime.GOOS != "windows" {
		return errors.New("auto-start is only set up for Windows; on a Mac add the program to Login Items")
	}
	dir := filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "Startup")
	launcher := filepath.Join(dir, "cdjhue.cmd")
	switch arg {
	case "on":
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		body := "@echo off\r\nstart \"Night Owl\" /min \"" + exe + "\" -open=false -retry\r\n"
		if err := os.WriteFile(launcher, []byte(body), 0o644); err != nil {
			return err
		}
		fmt.Printf("Night Owl will now start when you sign in to Windows.\nLauncher: %s\nIt runs this copy: %s (run this again if you move the program).\n", launcher, exe)
	case "off":
		if err := os.Remove(launcher); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Println("Night Owl will no longer start with Windows.")
	default:
		if _, err := os.Stat(launcher); err == nil {
			fmt.Println("Auto-start is on. Use: cdjhue autostart off")
		} else {
			fmt.Println("Auto-start is off. Use: cdjhue autostart on")
		}
	}
	return nil
}

func run(store *Store, bridgeIP, uiAddr string, open, takeover, retry bool, linkNumber int) error {
	log.Print(appName)
	if url := alreadyRunning(uiAddr); url != "" {
		log.Printf("Night Owl is already running on this computer; its control page is %s", url)
		log.Printf("close that copy first if you want to start a new one")
		if open {
			openBrowser(url)
		}
		return errors.New("not starting a second copy")
	}
	log.Printf("settings file: %s", store.path)
	hue, areas, err := connect(store, bridgeIP)
	for tries := 0; err != nil && retry && store.Config().Username != "" && tries < 60; tries++ {
		log.Printf("%v; trying again in 5 s", err)
		time.Sleep(5 * time.Second)
		hue, areas, err = connect(store, bridgeIP)
	}
	if err != nil {
		return err
	}
	hue.appID() // cache it before any goroutine needs it
	cfg := store.Config()
	var area *Area
	for i := range areas {
		if areas[i].ID == cfg.AreaID {
			area = &areas[i]
		}
	}
	if area == nil {
		if cfg.AreaID != "" {
			log.Printf("Entertainment area %q is no longer on the bridge", cfg.AreaName)
		}
		if area, err = chooseArea(store, areas); err != nil {
			return err
		}
	}
	if len(area.Channels) == 0 {
		return fmt.Errorf("Entertainment area %q has no lights", area.Name)
	}

	dj := NewDJLink()
	dj.StartOrRetry()
	ab := NewAbletonLink()
	go ab.Run()
	go warnIfSilent(dj, ab)
	tracker := NewTracker(dj)
	tracker.ab = ab
	tracker.phrases = NewPhraseStore()
	tracker.link = NewVCDJ(dj, tracker.phrases, tracker.OnStatus)
	tracker.link.forceNumber = linkNumber
	go tracker.Run()
	if len(store.Config().Areas) == 0 {
		_ = store.Update(func(c *Config) { c.Areas = []string{area.ID} })
	}
	first, err := buildShow(areas, store.Config().SelectedAreas())
	if err != nil { // the saved selection is stale: fall back to the main area
		_ = store.Update(func(c *Config) { c.Areas = []string{area.ID} })
		if first, err = buildShow(areas, []string{area.ID}); err != nil {
			return err
		}
	}
	app := &App{hue: hue, reconfig: make(chan struct{}, 1), store: store, dj: dj, tracker: tracker, link: tracker.link, linkAllowed: true, started: time.Now()}
	app.show.Store(first)
	app.paused = store.Config().Paused
	// Only the copy that is driving the lights joins the DJ network. A copy
	// standing by has no use for it, and the decks see one extra player
	// instead of several.
	go tracker.link.Run(func() (bool, string) {
		switch {
		case !store.Settings().DeckLink:
			return false, "off"
		case app.isPaused():
			return false, "on, but paused: it joins again when the sync is started"
		case !app.streaming.Load():
			return false, "on, but waiting: it joins when this copy is driving the lights"
		}
		return true, ""
	})

	ln, err := listenUI(uiAddr)
	if err != nil {
		return fmt.Errorf("cannot open the control page on %s: %w", uiAddr, err)
	}
	go func() { _ = http.Serve(ln, app.handler()) }()
	port := ln.Addr().(*net.TCPAddr).Port
	local := fmt.Sprintf("http://127.0.0.1:%d", port)
	log.Printf("control page: %s", local)
	for _, ip := range lanAddresses(store.Config().BridgeIP) {
		log.Printf("  from a phone on the same network: http://%s:%d", ip, port)
	}
	if open {
		openBrowser(local)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The most recently started app wins: this one takes the lights when it
	// starts, and if another app takes them later it waits its turn instead
	// of fighting back.
	for ctx.Err() == nil {
		sh := app.show.Load()
		// One session with the current selection of areas. It ends when
		// the program is stopping or the selection changes.
		sctx, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-app.reconfig:
				cancel()
			case <-sctx.Done():
			}
		}()
		followed := make(chan struct{})
		go func() { sh.follow(sctx, hue, app); close(followed) }()
		for sctx.Err() == nil {
			// Paused from the control page: leave the lights alone until it
			// is started again, then take them as a freshly started copy would.
			paused, changed := app.pauseState()
			if paused {
				app.lastErr.Store("")
				select {
				case <-sctx.Done():
				case <-changed:
					log.Printf("started again from the control page")
					takeover = true
				}
				continue
			}
			pctx, unpause := context.WithCancel(sctx)
			go func() {
				select {
				case <-changed:
					unpause()
				case <-pctx.Done():
				}
			}()
			err := stream(pctx, hue, app, sh, takeover)
			unpause()
			takeover = false
			app.streaming.Store(false)
			if sctx.Err() != nil {
				break
			}
			if app.isPaused() {
				_ = hue.StopStream(sh.stream.ID)
				log.Printf("paused from the control page: the lights are handed back")
				continue
			}
			if err != nil {
				app.lastErr.Store(err.Error())
			}
			if errors.Is(err, errBusy) {
				log.Printf("another app is driving these lights: waiting for it to finish")
				for sctx.Err() == nil && !app.isPaused() {
					if state, serr := hue.AreaState(sh.stream.ID); serr == nil && state != "other" {
						break
					}
					select {
					case <-sctx.Done():
					case <-changed:
					case <-time.After(3 * time.Second):
					}
				}
				continue
			}
			log.Printf("light stream stopped (%v); retrying in 2 s", err)
			select {
			case <-sctx.Done():
			case <-changed:
			case <-time.After(2 * time.Second):
			}
		}
		cancel()
		<-followed
		_ = hue.StopStream(sh.stream.ID)
		if ctx.Err() != nil {
			break
		}
		// The selection changed: lay the room out again and carry on.
		log.Printf("the selected areas changed: setting the lights up again")
		for ctx.Err() == nil {
			areas, err := hue.Areas()
			if err == nil {
				var next *Show
				if next, err = buildShow(areas, store.Config().SelectedAreas()); err == nil {
					app.show.Store(next)
					log.Printf("lights: %s", next.describe())
					break
				}
			}
			log.Printf("cannot set the lights up (%v); trying again in 5 s", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}
	log.Printf("stopping: handing the lights back")
	return nil
}

// stream runs one streaming session: 50 frames a second until it fails or
// the app is asked to stop.
func stream(ctx context.Context, hue *Hue, app *App, sh *Show, takeover bool) error {
	area := sh.stream
	if err := hue.StartStream(area.ID, takeover); err != nil {
		return fmt.Errorf("bridge did not start streaming: %w", err)
	}
	time.Sleep(250 * time.Millisecond) // give the bridge a moment to open its streaming port
	conn, err := hue.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	app.streaming.Store(true)
	app.lastErr.Store("")
	log.Printf("streaming to %q (%d channels)", area.Name, len(area.Channels))

	// Watch for another app taking the area over.
	lost := make(chan error, 1)
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-t.C:
				switch state, err := hue.AreaState(area.ID); {
				case err != nil:
				case state == "other":
					lost <- errBusy
					return
				case state == "free":
					lost <- errors.New("the bridge ended the stream")
					return
				}
			}
		}
	}()

	// 50 frames a second, plus one sent at the exact moment each beat's
	// pulse begins, so the start of a flash is not held back by up to a
	// whole frame interval.
	const interval = 20 * time.Millisecond
	timer := time.NewTimer(0)
	defer timer.Stop()
	var seq uint8
	cols := make([]RGB, len(sh.streamCh))
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-lost:
			return err
		case <-timer.C:
		}
		now := time.Now()
		s := app.store.Settings()
		ph := app.tracker.Phase(now, s)
		all := sh.engine.Render(now, ph, s)
		for i, at := range sh.streamIdx {
			cols[i] = all[at]
		}
		if _, err := conn.Write(frame(seq, area.ID, sh.streamCh, cols)); err != nil {
			return err
		}
		seq++
		wait := interval
		if ph.Locked {
			if toBeat := secs((1 - ph.Frac) * ph.Period); toBeat > time.Millisecond && toBeat < interval {
				wait = toBeat + 300*time.Microsecond // land just after the boundary
			}
		}
		timer.Reset(wait - time.Since(now))
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
