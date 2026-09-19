// Package dashboards stores the interactive pages an agent builds for a
// person to look at: one HTML file, a handful of JSON data files beside it,
// and — optionally — a recipe that keeps the data current.
//
// Filesystem, not the store's database: a client renders straight off
// <id>/index.html and <id>/data/<name>.json without going through the API,
// so the API and the on-disk layout have to be the same thing anyway. Two
// callers write here — the "dashboard" tool an agent holds, and the
// GET/DELETE/PUT-reference routes a client uses — which is the whole reason
// this is its own package rather than living inside either one.
package dashboards

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/loopinstall"
)

// Limits an agent can hit and needs to be told about in words it can act on,
// not a stack trace two calls later.
const (
	MaxHTMLBytes      = 512 << 10
	MaxDataValueBytes = 2 << 20
	MaxDataFiles      = 32
	MaxLiveNames      = 16
	MaxTitleChars     = 120
	MaxDescChars      = 500
)

var (
	// No underscore in the character class at all, which already keeps an id
	// from ever being "_kit" — the reserved check below exists only to say so
	// in words instead of via a regex someone has to read to understand why
	// their id was rejected.
	idPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	livePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
)

// Sentinel errors, so a caller (the tool, the API) can tell "you asked for
// something malformed" apart from "that thing does not exist" apart from
// "the disk is having a problem" without parsing a message.
var (
	ErrInvalidID   = errors.New("invalid dashboard id")
	ErrInvalidName = errors.New("invalid data file name")
	ErrNotFound    = errors.New("dashboard not found")
)

// Refresh is the schedule that keeps a dashboard's data current, and the
// brief telling the recipe's ask step what "current" means for this one.
type Refresh struct {
	Every string `json:"every,omitempty"`
	Cron  string `json:"cron,omitempty"`
	Brief string `json:"brief"`
}

// Meta is everything about a dashboard except its HTML and data — what
// action 'list' returns, and what rides alongside the HTML from 'get'.
type Meta struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Agent       string   `json:"agent,omitempty"`
	CreatedAt   string   `json:"createdAt"`
	UpdatedAt   string   `json:"updatedAt"`
	HTMLVersion int      `json:"htmlVersion"`
	DataVersion int      `json:"dataVersion"`
	Data        []string `json:"data"`
	Live        []string `json:"live,omitempty"`
	Refresh     *Refresh `json:"refresh,omitempty"`
	// Pinned and Archived are never omitempty: a client rendering the tab
	// strip needs to see "false" on every dashboard it never touched, not
	// just on the ones an operator explicitly reset.
	Pinned     bool   `json:"pinned"`
	Archived   bool   `json:"archived"`
	ArchivedAt string `json:"archivedAt,omitempty"`
}

// SaveInput is one 'save' call. A field's zero value and its absence mean
// different things (clearing the description on purpose vs. leaving it
// alone), so presence is carried alongside the value rather than inferred
// from it — the *Set fields are that presence.
type SaveInput struct {
	ID             string
	Title          string
	Description    string
	DescriptionSet bool
	HTML           string
	HTMLSet        bool
	// Data holds already-decoded JSON values (map[string]any, []any, string,
	// number, bool, nil, ...) — whatever the caller's JSON body produced.
	Data       map[string]any
	Live       []string
	LiveSet    bool
	Refresh    *Refresh // meaningful only when RefreshSet; nil there means "remove"
	RefreshSet bool
	// Agent is who is saving, for Meta.Agent. Empty means unknown (a caller
	// with no agent identity, e.g. a bare API client) — never overwrites a
	// previously recorded agent with blank.
	Agent string
}

// Root is where every dashboard lives. Honors the same data dir as every
// other on-disk KARMAX state, so `KARMAX_DATA_DIR` moves this along with it.
func Root() string { return filepath.Join(loopinstall.DataDir(), "dashboards") }

func dashboardDir(id string) string   { return filepath.Join(Root(), id) }
func htmlPath(id string) string       { return filepath.Join(dashboardDir(id), "index.html") }
func metaPath(id string) string       { return filepath.Join(dashboardDir(id), "dashboard.json") }
func dataDir(id string) string        { return filepath.Join(dashboardDir(id), "data") }
func dataPath(id, name string) string { return filepath.Join(dataDir(id), name+".json") }

func validateID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: empty", ErrInvalidID)
	}
	if strings.HasPrefix(id, "_") {
		return fmt.Errorf("%w: %q starts with \"_\", which is reserved for KARMAX's own use (the component kit lives at _kit)", ErrInvalidID, id)
	}
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%w: %q must match ^[a-z0-9][a-z0-9-]{0,63}$ (lowercase letters, digits, hyphens)", ErrInvalidID, id)
	}
	return nil
}

func validateDataName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%w: %q must match ^[a-z0-9][a-z0-9_-]{0,63}$ (lowercase letters, digits, hyphens, underscores)", ErrInvalidName, name)
	}
	return nil
}

func validateLiveName(name string) error {
	if !livePattern.MatchString(name) {
		return fmt.Errorf("live name %q must match ^[a-z0-9][a-z0-9-]{0,31}$", name)
	}
	return nil
}

// slugify turns a title into a candidate id. It can fail — a title that is
// all punctuation or emoji has no id hiding inside it — and failing loudly
// beats silently saving as "dashboard" and colliding with the next one.
func slugify(title string) (string, error) {
	var b strings.Builder
	lastDash := true // true so a leading run of junk doesn't start with '-'
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	id := strings.Trim(b.String(), "-")
	if len(id) > 64 {
		id = strings.Trim(id[:64], "-")
	}
	if id == "" {
		return "", fmt.Errorf("could not derive an id from title %q — give one explicitly", title)
	}
	return id, nil
}

// perID serialises every write to one dashboard behind a single mutex, kept
// process-wide (not per Store, there is no Store) so a save via the tool and
// a delete via the API can never interleave on the same id — the failure
// this exists to prevent is a meta.json left half-updated by one call while
// the other is mid-write to the same file.
var (
	locksMu sync.Mutex
	locks   = map[string]*sync.Mutex{}
)

func lockFor(id string) *sync.Mutex {
	locksMu.Lock()
	defer locksMu.Unlock()
	l, ok := locks[id]
	if !ok {
		l = &sync.Mutex{}
		locks[id] = l
	}
	return l
}

// atomicWrite writes to a temp file in the same directory and renames it into
// place. Never a direct write: a reader (the client rendering the page, an
// API request landing mid-save) must never observe a half-written file, and a
// crash between the two write calls a plain os.WriteFile makes internally
// must never leave a truncated one where the good version used to be.
func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func readMeta(id string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(metaPath(id))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	return m, nil
}

func writeMeta(m Meta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWrite(metaPath(m.ID), b)
}

// listDataNames reads the truth off disk rather than trusting Meta.Data,
// which is a cache of exactly this and must never drift from what a client
// can actually GET.
func listDataNames(id string) []string {
	entries, err := os.ReadDir(dataDir(id))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(out)
	return out
}

// now is nanosecond-precision RFC3339 (still valid RFC3339 — fractional
// seconds are part of the grammar), not second-precision: two saves in the
// same second are common (a refresh recipe writing several dashboards back
// to back) and List's newest-first order would otherwise tie between them.
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Save creates a dashboard or updates an existing one. See SaveInput for what
// "provided" vs. "omitted" means for each field.
func Save(in SaveInput) (Meta, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		slug, err := slugify(in.Title)
		if err != nil {
			return Meta{}, err
		}
		id = slug
	}
	if err := validateID(id); err != nil {
		return Meta{}, err
	}

	title := strings.TrimSpace(in.Title)
	if title == "" {
		return Meta{}, fmt.Errorf("title is required")
	}
	if len(title) > MaxTitleChars {
		return Meta{}, fmt.Errorf("title is %d characters, over the %d limit", len(title), MaxTitleChars)
	}
	if in.DescriptionSet && len(in.Description) > MaxDescChars {
		return Meta{}, fmt.Errorf("description is %d characters, over the %d limit", len(in.Description), MaxDescChars)
	}
	if in.HTMLSet && len(in.HTML) > MaxHTMLBytes {
		return Meta{}, fmt.Errorf("html is %d bytes, over the %d KB limit", len(in.HTML), MaxHTMLBytes/1024)
	}
	if in.LiveSet {
		if len(in.Live) > MaxLiveNames {
			return Meta{}, fmt.Errorf("live names %d, over the %d limit", len(in.Live), MaxLiveNames)
		}
		for _, l := range in.Live {
			if err := validateLiveName(l); err != nil {
				return Meta{}, err
			}
		}
	}
	if in.RefreshSet && in.Refresh != nil {
		if err := validateRefresh(in.Refresh); err != nil {
			return Meta{}, err
		}
	}

	// Marshalled and size-checked before anything is written, so a bad entry
	// in a five-entry 'data' map fails the whole call instead of leaving the
	// first four written and the rest silently missing.
	marshaled := map[string][]byte{}
	for name, val := range in.Data {
		if err := validateDataName(name); err != nil {
			return Meta{}, err
		}
		b, err := json.Marshal(val)
		if err != nil {
			return Meta{}, fmt.Errorf("data %q does not marshal to JSON: %w", name, err)
		}
		if len(b) > MaxDataValueBytes {
			return Meta{}, fmt.Errorf("data %q is %d bytes, over the %d MB limit", name, len(b), MaxDataValueBytes/(1<<20))
		}
		marshaled[name] = b
	}

	lock := lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	existing, err := readMeta(id)
	exists := err == nil
	if !exists && !in.HTMLSet {
		return Meta{}, fmt.Errorf("dashboard %q does not exist yet — html is required to create one", id)
	}

	if len(marshaled) > 0 {
		before := map[string]bool{}
		for _, n := range listDataNames(id) {
			before[n] = true
		}
		total := len(before)
		for name := range marshaled {
			if !before[name] {
				total++
			}
		}
		if total > MaxDataFiles {
			return Meta{}, fmt.Errorf("that would leave %d data files on this dashboard, over the %d limit", total, MaxDataFiles)
		}
	}

	meta := existing
	if !exists {
		meta = Meta{ID: id, CreatedAt: now()}
	}
	meta.Title = title
	if in.DescriptionSet {
		meta.Description = in.Description
	}
	if strings.TrimSpace(in.Agent) != "" {
		meta.Agent = in.Agent
	}

	if in.HTMLSet {
		if err := atomicWrite(htmlPath(id), []byte(in.HTML)); err != nil {
			return Meta{}, fmt.Errorf("writing html: %w", err)
		}
		meta.HTMLVersion++
	}
	for name, b := range marshaled {
		if err := atomicWrite(dataPath(id, name), b); err != nil {
			return Meta{}, fmt.Errorf("writing data %q: %w", name, err)
		}
	}
	if len(marshaled) > 0 {
		meta.DataVersion++
	}
	meta.Data = listDataNames(id)

	if in.LiveSet {
		meta.Live = in.Live
	}

	if in.RefreshSet {
		if in.Refresh == nil {
			meta.Refresh = nil
			if err := removeRefreshRecipe(id); err != nil {
				return Meta{}, fmt.Errorf("removing the refresh recipe: %w", err)
			}
		} else {
			meta.Refresh = in.Refresh
			if meta.Archived {
				// Recorded, not installed: an archived dashboard's recipe
				// stays removed, or the ask step this same recipe used to run
				// would resurrect the schedule the operator just put away.
				if err := removeRefreshRecipe(id); err != nil {
					return Meta{}, fmt.Errorf("removing the refresh recipe: %w", err)
				}
			} else if err := writeRefreshRecipe(id, meta.Title, in.Refresh, meta.Data); err != nil {
				return Meta{}, err
			}
		}
	}

	meta.UpdatedAt = now()
	if err := writeMeta(meta); err != nil {
		return Meta{}, fmt.Errorf("writing dashboard.json: %w", err)
	}
	return meta, nil
}

// SetFlags updates a dashboard's pin/archive state. Either argument nil
// means "leave that one alone", so pinning never has to first read whether
// the dashboard is archived. Versions and UpdatedAt are untouched — nothing
// about the dashboard's content changed, and a client showing "updated N
// ago" would be lying if this bumped it.
//
// Archiving removes the refresh recipe while keeping it recorded on
// Meta.Refresh: an archived dashboard is put away, not gone, but its
// schedule must stop running (nobody is looking at data it would still be
// spending tool calls to refresh). Un-archiving restores that recipe, if
// there was one.
func SetFlags(id string, pinned *bool, archived *bool) (Meta, error) {
	if err := validateID(id); err != nil {
		return Meta{}, err
	}

	lock := lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	meta, err := readMeta(id)
	if err != nil {
		return Meta{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	if pinned != nil {
		meta.Pinned = *pinned
	}

	// Compared against the current state rather than assigned unconditionally,
	// so a PATCH that repeats the dashboard's existing archived value doesn't
	// re-stamp ArchivedAt or churn the recipe file on every call.
	if archived != nil && *archived != meta.Archived {
		meta.Archived = *archived
		if *archived {
			meta.ArchivedAt = now()
			if err := removeRefreshRecipe(id); err != nil {
				return Meta{}, fmt.Errorf("removing the refresh recipe: %w", err)
			}
		} else {
			meta.ArchivedAt = ""
			if meta.Refresh != nil {
				if err := writeRefreshRecipe(id, meta.Title, meta.Refresh, meta.Data); err != nil {
					return Meta{}, err
				}
			}
		}
	}

	if err := writeMeta(meta); err != nil {
		return Meta{}, fmt.Errorf("writing dashboard.json: %w", err)
	}
	return meta, nil
}

// SetData writes one data file on an already-existing dashboard, without
// touching its HTML — the call the refresh recipe's ask step makes.
func SetData(id, name string, value any, agent string) (Meta, error) {
	if err := validateID(id); err != nil {
		return Meta{}, err
	}
	if err := validateDataName(name); err != nil {
		return Meta{}, err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return Meta{}, fmt.Errorf("value does not marshal to JSON: %w", err)
	}
	if len(b) > MaxDataValueBytes {
		return Meta{}, fmt.Errorf("value is %d bytes, over the %d MB limit", len(b), MaxDataValueBytes/(1<<20))
	}

	lock := lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	meta, err := readMeta(id)
	if err != nil {
		return Meta{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	existingNames := listDataNames(id)
	isNew := true
	for _, n := range existingNames {
		if n == name {
			isNew = false
			break
		}
	}
	if isNew && len(existingNames)+1 > MaxDataFiles {
		return Meta{}, fmt.Errorf("that would leave %d data files on this dashboard, over the %d limit", len(existingNames)+1, MaxDataFiles)
	}

	if err := atomicWrite(dataPath(id, name), b); err != nil {
		return Meta{}, fmt.Errorf("writing data %q: %w", name, err)
	}
	meta.DataVersion++
	meta.Data = listDataNames(id)
	if strings.TrimSpace(agent) != "" {
		meta.Agent = agent
	}
	meta.UpdatedAt = now()
	if err := writeMeta(meta); err != nil {
		return Meta{}, fmt.Errorf("writing dashboard.json: %w", err)
	}
	return meta, nil
}

// Get returns one dashboard's metadata and HTML.
func Get(id string) (Meta, string, error) {
	if err := validateID(id); err != nil {
		return Meta{}, "", err
	}
	meta, err := readMeta(id)
	if err != nil {
		return Meta{}, "", fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	html, err := os.ReadFile(htmlPath(id))
	if err != nil {
		return Meta{}, "", fmt.Errorf("dashboard %q has no html: %w", id, err)
	}
	return meta, string(html), nil
}

// GetData returns one data file's raw JSON bytes, unparsed — the API streams
// this straight through as the response body.
func GetData(id, name string) ([]byte, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	if err := validateDataName(name); err != nil {
		return nil, err
	}
	if _, err := readMeta(id); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	b, err := os.ReadFile(dataPath(id, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: dashboard %q has no data file %q", ErrNotFound, id, name)
		}
		return nil, err
	}
	return b, nil
}

// List returns every dashboard's metadata, newest-updated first.
func List() ([]Meta, error) {
	entries, err := os.ReadDir(Root())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Meta
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		m, err := readMeta(e.Name())
		if err != nil {
			continue // a folder without a valid dashboard.json is not a dashboard
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}

// Delete removes a dashboard's folder and its refresh recipe, if it had one.
func Delete(id string) error {
	if err := validateID(id); err != nil {
		return err
	}

	lock := lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	if _, err := readMeta(id); err != nil {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err := os.RemoveAll(dashboardDir(id)); err != nil {
		return err
	}
	// Best-effort past this point: the dashboard is already gone from the
	// caller's point of view, and a stray recipe file refers to a dashboard
	// that no longer exists rather than corrupting one that does.
	return removeRefreshRecipe(id)
}
