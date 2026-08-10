package recipes

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Dir is where recipes live. Dropping a file here is the whole install step.
func Dir() string {
	if d := strings.TrimSpace(os.Getenv("KARMAX_RECIPES_DIR")); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "recipes"
	}
	return filepath.Join(home, ".karmax", "recipes")
}

// Loaded is a recipe and whatever was wrong with it.
type Loaded struct {
	Recipe *Recipe
	Err    error
	Path   string
}

// LoadAll reads every recipe in dir. A broken file is reported and skipped, not
// fatal — one bad recipe must not stop the others.
func LoadAll(dir string) []Loaded {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Loaded
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			out = append(out, Loaded{Path: path, Err: err})
			continue
		}
		r, err := Parse(path, data)
		out = append(out, Loaded{Path: path, Recipe: r, Err: err})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Watcher reloads recipes when the directory changes.
//
// Polled rather than inotify: the check is a directory stat every few seconds,
// it works the same on every platform, and it has no file-descriptor limit to
// run into. A recipe author saves a file and it is live within seconds, which
// is the only property that matters here.
type Watcher struct {
	dir      string
	log      *zap.Logger
	interval time.Duration

	mu      sync.Mutex
	stamps  map[string]time.Time
	onApply func([]Loaded)
}

func NewWatcher(dir string, log *zap.Logger, onApply func([]Loaded)) *Watcher {
	return &Watcher{dir: dir, log: log, interval: 3 * time.Second,
		stamps: map[string]time.Time{}, onApply: onApply}
}

func (w *Watcher) Start(ctx context.Context) {
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		w.log.Warn("could not create the recipes directory", zap.String("dir", w.dir), zap.Error(err))
		return
	}
	w.scan(true)
	go func() {
		t := time.NewTicker(w.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.scan(false)
			}
		}
	}()
}

func (w *Watcher) scan(first bool) {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	now := map[string]time.Time{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		now[e.Name()] = info.ModTime()
	}

	w.mu.Lock()
	changed := len(now) != len(w.stamps)
	if !changed {
		for name, at := range now {
			if prev, ok := w.stamps[name]; !ok || !prev.Equal(at) {
				changed = true
				break
			}
		}
	}
	w.stamps = now
	w.mu.Unlock()

	if !changed && !first {
		return
	}
	loaded := LoadAll(w.dir)
	for _, l := range loaded {
		if l.Err != nil {
			// Reported where the author will see it, with the line and a fix.
			w.log.Error("recipe has a problem and was not loaded\n" + l.Err.Error())
		}
	}
	w.onApply(loaded)
}

// Valid returns the recipes that parsed and are enabled.
func Valid(loaded []Loaded) []*Recipe {
	var out []*Recipe
	for _, l := range loaded {
		if l.Err == nil && l.Recipe != nil && l.Recipe.IsEnabled() {
			out = append(out, l.Recipe)
		}
	}
	return out
}
