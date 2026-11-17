package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// DefaultProjectAPI is Folding@home's public project endpoint; the
	// project id is appended to it.
	DefaultProjectAPI = "https://api.foldingathome.org/project/"
	// ProjectTimeout bounds one metadata request.
	ProjectTimeout = 3 * time.Second

	projectStatsURL = "https://stats.foldingathome.org/project/"
	// projectRetry is how long a failed lookup is remembered, so an offline
	// machine is not asked again on every status poll.
	projectRetry = time.Minute
	// maxProjectBody bounds the response read: descriptions are a few KiB.
	maxProjectBody = 1 << 20
	maxSummaryLen  = 200
	maxFieldLen    = 80
)

// ErrProjectUnavailable is returned while a recent failed lookup is still
// remembered: nothing was requested.
var ErrProjectUnavailable = errors.New("gui: project metadata unavailable, retrying later")

// Project is what Folding@home publishes about a project. Every field but
// ID may be empty.
type Project struct {
	ID int
	// Cause is the research area in words ("Alzheimer's Disease"); empty
	// when the project does not declare one.
	Cause       string
	Institution string
	Manager     string
	// Summary is the first sentence of the description, as plain text.
	Summary string
}

// ProjectTitle is how a project is named in the UI.
func ProjectTitle(id int) string {
	return fmt.Sprintf("Project #%d", id)
}

// ProjectURL is the project's public page.
func ProjectURL(id int) string {
	return projectStatsURL + strconv.Itoa(id)
}

// ProjectClient fetches project metadata and caches it in memory by project
// id. The cache is not bounded: a session sees a handful of projects.
//
// A ProjectClient is safe for concurrent use.
type ProjectClient struct {
	base  string
	http  *http.Client
	retry time.Duration
	now   func() time.Time

	mu     sync.Mutex
	cache  map[int]Project
	failed map[int]time.Time
}

// NewProjectClient returns a client for the API at baseURL, to which the
// project id is appended. Empty means DefaultProjectAPI.
func NewProjectClient(baseURL string) *ProjectClient {
	if baseURL == "" {
		baseURL = DefaultProjectAPI
	}
	return &ProjectClient{
		base:   baseURL,
		http:   &http.Client{Timeout: ProjectTimeout},
		retry:  projectRetry,
		now:    time.Now,
		cache:  map[int]Project{},
		failed: map[int]time.Time{},
	}
}

// Cached returns the project if a lookup already succeeded. It never blocks
// on the network.
func (c *ProjectClient) Cached(id int) (Project, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.cache[id]
	return p, ok
}

// Lookup returns the project's metadata, from the cache when it can. It
// blocks for at most ProjectTimeout. After a failure the same id returns
// ErrProjectUnavailable, without a request, for a minute.
func (c *ProjectClient) Lookup(ctx context.Context, id int) (Project, error) {
	if id <= 0 {
		return Project{}, fmt.Errorf("gui: invalid project id %d", id)
	}
	c.mu.Lock()
	if p, ok := c.cache[id]; ok {
		c.mu.Unlock()
		return p, nil
	}
	if at, ok := c.failed[id]; ok && c.now().Sub(at) < c.retry {
		c.mu.Unlock()
		return Project{}, ErrProjectUnavailable
	}
	c.mu.Unlock()

	p, err := c.fetch(ctx, id)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		// A caller that gave up says nothing about the network.
		if ctx.Err() == nil {
			c.failed[id] = c.now()
		}
		return Project{}, err
	}
	delete(c.failed, id)
	c.cache[id] = p
	return p, nil
}

func (c *ProjectClient) fetch(ctx context.Context, id int) (Project, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+strconv.Itoa(id), nil)
	if err != nil {
		return Project{}, fmt.Errorf("gui: project %d: %w", id, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ecofold-gui")
	resp, err := c.http.Do(req)
	if err != nil {
		return Project{}, fmt.Errorf("gui: project %d: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Project{}, fmt.Errorf("gui: project %d: HTTP %d", id, resp.StatusCode)
	}
	var raw struct {
		Cause       string `json:"cause"`
		Institution string `json:"institution"`
		Manager     string `json:"manager"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxProjectBody)).Decode(&raw); err != nil {
		return Project{}, fmt.Errorf("gui: project %d: unreadable metadata: %w", id, err)
	}
	return Project{
		ID:          id,
		Cause:       causeName(raw.Cause),
		Institution: truncate(plainText(raw.Institution), maxFieldLen),
		Manager:     truncate(plainText(raw.Manager), maxFieldLen),
		Summary:     summarize(raw.Description),
	}, nil
}

// causeNames are the API's cause slugs that do not read well capitalized.
var causeNames = map[string]string{
	"alzheimers":  "Alzheimer's Disease",
	"parkinsons":  "Parkinson's Disease",
	"huntingtons": "Huntington's Disease",
	"covid-19":    "COVID-19",
	"unspecified": "",
}

// causeName turns the API's cause slug ("alzheimers", "cancer") into words.
func causeName(slug string) string {
	slug = strings.ToLower(plainText(slug))
	if name, ok := causeNames[slug]; ok {
		return name
	}
	words := strings.FieldsFunc(slug, func(r rune) bool { return r == '-' || r == '_' || unicode.IsSpace(r) })
	for i, w := range words {
		r, n := utf8.DecodeRuneInString(w)
		words[i] = string(unicode.ToUpper(r)) + w[n:]
	}
	return truncate(strings.Join(words, " "), maxFieldLen)
}

// summarize reduces the API's HTML description to its first sentence.
func summarize(desc string) string {
	text := plainText(desc)
	if i := strings.Index(text, ". "); i >= 0 {
		text = text[:i+1]
	}
	return truncate(text, maxSummaryLen)
}

// plainText drops HTML tags and entities and collapses white space.
func plainText(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
			b.WriteByte(' ')
		case r == '>' && inTag:
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	// Fields also splits on the non-breaking spaces the entities decode to.
	return strings.Join(strings.Fields(html.UnescapeString(b.String())), " ")
}

// truncate cuts s to at most n runes, marking the cut.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
