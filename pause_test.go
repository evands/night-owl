package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testApp is an app with a room of three lights and no bridge or decks.
func testApp(t *testing.T) *App {
	t.Helper()
	store, err := loadStore(filepath.Join(t.TempDir(), "cdjhue.json"))
	if err != nil {
		t.Fatal(err)
	}
	room := Area{ID: "a", Name: "Room", Channels: []Channel{
		{ID: 0, X: -1, LightID: "L1", Name: "Left"}, {ID: 1, X: 0, LightID: "L2", Name: "Middle"}, {ID: 2, X: 1, LightID: "L3", Name: "Right"},
	}, Lights: []AreaLight{{ID: "L1", Name: "Left", X: -1}, {ID: "L2", Name: "Middle"}, {ID: "L3", Name: "Right", X: 1}}}
	sh, err := buildShow([]Area{room}, []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	dj := NewDJLink()
	app := &App{store: store, dj: dj, tracker: NewTracker(dj), reconfig: make(chan struct{}, 1), started: time.Now()}
	app.show.Store(sh)
	app.paused = store.Config().Paused
	return app
}

func TestPauseFromThePage(t *testing.T) {
	app := testApp(t)
	srv := httptest.NewServer(app.handler())
	defer srv.Close()

	paused := func() bool {
		t.Helper()
		res, err := http.Get(srv.URL + "/api/state")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var d struct {
			Status struct {
				Paused bool `json:"paused"`
			} `json:"status"`
		}
		if err := json.NewDecoder(res.Body).Decode(&d); err != nil {
			t.Fatal(err)
		}
		return d.Status.Paused
	}
	post := func(body string) int {
		t.Helper()
		res, err := http.Post(srv.URL+"/api/pause", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	if paused() {
		t.Fatal("a new installation must start running")
	}
	was, changed := app.pauseState()
	if was {
		t.Fatal("not paused yet")
	}
	if code := post(`{"paused":true}`); code != http.StatusOK {
		t.Fatalf("pause: status %d", code)
	}
	select {
	case <-changed:
	default:
		t.Fatal("pausing must wake whoever is waiting on the change")
	}
	if !paused() || !app.isPaused() {
		t.Fatal("the page must see it paused")
	}
	// It is remembered: a restart comes back paused.
	again, err := loadStore(app.store.path)
	if err != nil || !again.Config().Paused {
		t.Fatalf("paused must be saved: %v, %+v", err, again.Config().Paused)
	}
	// Asking for what is already so changes nothing and wakes nobody.
	_, changed = app.pauseState()
	if code := post(`{"paused":true}`); code != http.StatusOK {
		t.Fatalf("pause again: status %d", code)
	}
	select {
	case <-changed:
		t.Fatal("no change, so nobody should be woken")
	default:
	}
	if code := post(`{"paused":false}`); code != http.StatusOK || paused() {
		t.Fatalf("start again: status %d, paused %v", code, paused())
	}
	if again, _ = loadStore(app.store.path); again.Config().Paused {
		t.Fatal("starting again must be saved too")
	}
	if res, _ := http.Get(srv.URL + "/api/pause"); res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET must be refused, got %d", res.StatusCode)
	}
	if code := post(`nonsense`); code != http.StatusBadRequest {
		t.Fatalf("bad body: status %d", code)
	}
}
