package gui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// alzheimersJSON is the shape of api.foldingathome.org/project/18264.
const alzheimersJSON = `{"manager":"Justin Miller","cause":"alzheimers",
"description":"<p>Alzheimer&#39;s disease&nbsp;is a significant cause of death and memory loss and there are no effective treatments. One of the late hallmarks is tau.</p>\n\n<p>More text.</p>",
"thumb":"","url":"","institution":"University of Pennsylvania","modified":"2024-07-22 14:23:12","projects":"18251-18264"}`

// projectAPI is a fake of the Folding@home project API. It counts the
// requests per path.
type projectAPI struct {
	srv *httptest.Server

	mu      sync.Mutex
	hits    map[string]int
	handler http.HandlerFunc
}

func newProjectAPI(t *testing.T, handler http.HandlerFunc) *projectAPI {
	t.Helper()
	api := &projectAPI{hits: map[string]int{}, handler: handler}
	api.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		api.hits[r.URL.Path]++
		h := api.handler
		api.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(api.srv.Close)
	return api
}

func (a *projectAPI) client() *ProjectClient {
	return NewProjectClient(a.srv.URL + "/project/")
}

func (a *projectAPI) count(path string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hits[path]
}

func (a *projectAPI) handle(h http.HandlerFunc) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handler = h
}

func serveJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}
}

func TestProjectLookupAndCache(t *testing.T) {
	api := newProjectAPI(t, serveJSON(alzheimersJSON))
	c := api.client()

	if _, ok := c.Cached(18264); ok {
		t.Fatal("caché con datos antes de consultar")
	}
	p, err := c.Lookup(bg, 18264)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	want := Project{
		ID:          18264,
		Cause:       "Alzheimer's Disease",
		Institution: "University of Pennsylvania",
		Manager:     "Justin Miller",
		Summary:     "Alzheimer's disease is a significant cause of death and memory loss and there are no effective treatments.",
	}
	if p != want {
		t.Fatalf("proyecto = %+v", p)
	}
	if ProjectURL(p.ID) != "https://stats.foldingathome.org/project/18264" || ProjectTitle(p.ID) != "Project #18264" {
		t.Fatalf("enlace o título = %q / %q", ProjectURL(p.ID), ProjectTitle(p.ID))
	}

	// The second lookup and Cached do not touch the network.
	if again, err := c.Lookup(bg, 18264); err != nil || again != want {
		t.Fatalf("segunda consulta = %+v, %v", again, err)
	}
	if cached, ok := c.Cached(18264); !ok || cached != want {
		t.Fatalf("Cached = %+v, %v", cached, ok)
	}
	if n := api.count("/project/18264"); n != 1 {
		t.Fatalf("peticiones = %d, se esperaba 1", n)
	}

	// The cache is per project id.
	if _, err := c.Lookup(bg, 18265); err != nil {
		t.Fatalf("Lookup de otro proyecto: %v", err)
	}
	if n := api.count("/project/18265"); n != 1 {
		t.Fatalf("peticiones del otro proyecto = %d", n)
	}

	if _, err := c.Lookup(bg, 0); err == nil {
		t.Fatal("se aceptó el proyecto 0")
	}
}

func TestProjectNotFoundIsRememberedThenRetried(t *testing.T) {
	api := newProjectAPI(t, http.NotFound)
	c := api.client()
	now := time.Unix(1_800_000_000, 0)
	c.now = func() time.Time { return now }

	if _, err := c.Lookup(bg, 404404); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("404: err = %v", err)
	}
	// Within the retry window nothing is requested.
	if _, err := c.Lookup(bg, 404404); !errors.Is(err, ErrProjectUnavailable) {
		t.Fatalf("segundo intento: err = %v", err)
	}
	if n := api.count("/project/404404"); n != 1 {
		t.Fatalf("peticiones = %d, se esperaba 1", n)
	}
	if _, ok := c.Cached(404404); ok {
		t.Fatal("un fallo quedó en la caché")
	}

	// After it, the API is asked again and a success is cached.
	api.handle(serveJSON(`{"cause":"cancer","institution":"MSKCC"}`))
	now = now.Add(projectRetry + time.Second)
	p, err := c.Lookup(bg, 404404)
	if err != nil || p.Cause != "Cancer" || p.Institution != "MSKCC" {
		t.Fatalf("tras el reintento = %+v, %v", p, err)
	}
	if n := api.count("/project/404404"); n != 2 {
		t.Fatalf("peticiones = %d, se esperaba 2", n)
	}
}

func TestProjectTimeout(t *testing.T) {
	release := make(chan struct{})
	api := newProjectAPI(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) }) // runs before the server closes
	c := api.client()
	c.http.Timeout = 50 * time.Millisecond

	start := time.Now()
	_, err := c.Lookup(bg, 18264)
	if err == nil {
		t.Fatal("la consulta no caducó")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("la consulta tardó %v", d)
	}
	if _, err := c.Lookup(bg, 18264); !errors.Is(err, ErrProjectUnavailable) {
		t.Fatalf("tras el timeout: err = %v", err)
	}

	// A caller that cancels is not a network failure: nothing is remembered.
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := c.Lookup(ctx, 777); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelado: err = %v", err)
	}
	if _, err := c.Lookup(ctx, 777); errors.Is(err, ErrProjectUnavailable) {
		t.Fatal("una cancelación se recordó como fallo")
	}
}

func TestProjectOfflineAndGarbage(t *testing.T) {
	// Nothing listens: the machine is offline.
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL + "/project/"
	srv.Close()
	if _, err := NewProjectClient(base).Lookup(bg, 18264); err == nil {
		t.Fatal("sin red no hubo error")
	}

	api := newProjectAPI(t, serveJSON(`<html>captive portal</html>`))
	if _, err := api.client().Lookup(bg, 18264); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("respuesta ilegible: err = %v", err)
	}
	// An oversized body is cut, not read whole.
	api.handle(serveJSON(`{"institution":"` + strings.Repeat("x", 2*maxProjectBody) + `"}`))
	if _, err := api.client().Lookup(bg, 18264); err == nil {
		t.Fatal("se leyó una respuesta enorme")
	}
}

func TestProjectText(t *testing.T) {
	for slug, want := range map[string]string{
		"alzheimers":    "Alzheimer's Disease",
		"parkinsons":    "Parkinson's Disease",
		"huntingtons":   "Huntington's Disease",
		"covid-19":      "COVID-19",
		"Cancer":        "Cancer",
		"high-priority": "High Priority",
		"kidney_cancer": "Kidney Cancer",
		"unspecified":   "",
		"":              "",
		"<b>ébola</b>":  "Ébola",
	} {
		if got := causeName(slug); got != want {
			t.Errorf("causeName(%q) = %q, se esperaba %q", slug, got, want)
		}
	}

	if got := summarize("<p>One&nbsp;sentence   only</p>"); got != "One sentence only" {
		t.Errorf("resumen = %q", got)
	}
	long := summarize("<p>" + strings.Repeat("palabra ", 100) + "</p>")
	if n := len([]rune(long)); n > maxSummaryLen || !strings.HasSuffix(long, "…") {
		t.Errorf("resumen largo: %d runas, %q", n, long)
	}
	if got := summarize(""); got != "" {
		t.Errorf("resumen vacío = %q", got)
	}

	v := View{Project: Project{ID: 1, Cause: "Cancer"}}
	if got := v.CauseLabel(); got != "Researching: Cancer" {
		t.Errorf("causa = %q", got)
	}
	if got := (View{}).CauseLabel(); got != "" {
		t.Errorf("causa sin proyecto = %q", got)
	}
}

// TestControllerAddsProjectMetadata: the view gets the metadata of the active
// project without the status waiting for it, and a failing API leaves the
// view connected with the bare project id.
func TestControllerAddsProjectMetadata(t *testing.T) {
	path := socketPath(t)
	d := newFakeDaemon()
	d.set(func(d *fakeDaemon) {
		d.status.FAH.Reachable, d.status.FAH.ObservedAt = true, time.Now()
		d.status.FAH.State, d.status.FAH.ActiveProjectID = "folding", 18264
	})
	startDaemon(t, path, d)

	var broken atomic.Bool
	broken.Store(true)
	api := newProjectAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		serveJSON(alzheimersJSON)(w, r)
	})
	projects := api.client()
	projects.retry = 0 // every poll may try again

	c := New(Config{SocketPath: path, Poll: 10 * time.Millisecond, Timeout: time.Second, Projects: projects})
	waitFor := func(what string, ok func(View) bool) View {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			c.Refresh(bg)
			if v := c.View(); ok(v) {
				return v
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("no se llegó a %q; vista = %+v", what, c.View())
		return View{}
	}

	// The API fails: connected, project id only.
	waitFor("consulta fallida", func(View) bool { return api.count("/project/18264") > 0 })
	if v := c.View(); !v.Connected || v.Status.FAH.ActiveProjectID != 18264 || v.Project != (Project{}) {
		t.Fatalf("con la API caída: %+v", v)
	}

	broken.Store(false)
	v := waitFor("metadatos", func(v View) bool { return v.Project.ID == 18264 })
	if !v.Connected || v.CauseLabel() != "Researching: Alzheimer's Disease" || v.Project.Institution != "University of Pennsylvania" {
		t.Fatalf("metadatos = %+v", v.Project)
	}

	// A new work unit: the old metadata must not be shown for it.
	d.set(func(d *fakeDaemon) { d.status.FAH.ActiveProjectID = 0 })
	c.Refresh(bg)
	if v := c.View(); v.Project != (Project{}) {
		t.Fatalf("sin proyecto activo quedó %+v", v.Project)
	}
	// Back to the known project: served from the cache, at once.
	hits := api.count("/project/18264")
	d.set(func(d *fakeDaemon) { d.status.FAH.ActiveProjectID = 18264 })
	c.Refresh(bg)
	if v := c.View(); v.Project.ID != 18264 || api.count("/project/18264") != hits {
		t.Fatalf("desde la caché: %+v (peticiones %d -> %d)", v.Project, hits, api.count("/project/18264"))
	}
}
