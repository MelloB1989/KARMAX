package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/MelloB1989/karmax/internal/loopinstall"
	"github.com/MelloB1989/karmax/internal/loopregistry"
	"github.com/MelloB1989/karmax/internal/recipes"
	"github.com/MelloB1989/karmax/internal/wasmloop"
)

// The registry API: what the desktop app's Loops screen needs and cannot get
// by shelling out to `karmax loops browse|install` — a prompt.
//
// Every write here goes through internal/loopregistry, the same package the
// CLI now calls, so "installed from the app" and "installed from the
// terminal" verify the identical thing (digest, then signature/trust for a
// workflow) and can never quietly drift apart.
//
// loopNamePattern reuses console_recipes.go's recipeNamePattern rather than
// declaring a second identical regex: a loop name is a recipe name, a
// workflow name, or a compiled loop's name, and all three share the same
// filesystem-safe shape (it ends up in a file path either way).
var loopNamePattern = recipeNamePattern

// registryEntryView is one registry.json entry as the app sees it: what it
// is, and — cross-referenced against this machine — whether it is already
// here and whether it is actually running.
type registryEntryView struct {
	Name             string   `json:"name"`
	Kind             string   `json:"kind"`
	Version          string   `json:"version"`
	Description      string   `json:"description"`
	Author           string   `json:"author"`
	Requires         []string `json:"requires"`
	ShipsWithEngine  bool     `json:"shipsWithEngine"`
	Installed        bool     `json:"installed"`
	InstalledVersion string   `json:"installedVersion"`
	Active           bool     `json:"active"`
	SourceURL        string   `json:"sourceUrl"`
}

// activeLoopNames is the set GET /api/loops currently reports — "active" for
// the registry view means exactly that, not merely installed.
func (s *Server) activeLoopNames() map[string]bool {
	out := map[string]bool{}
	if s.listLoops == nil {
		return out
	}
	for _, l := range s.listLoops() {
		out[l.Name] = true
	}
	return out
}

// installedWorkflowVersion reads the signed-loop lockfile directly rather
// than through loopregistry.InstalledNames (a bool set), because the
// registry view needs the VERSION a recipe carries no equivalent of.
func installedWorkflowVersion(name string) string {
	lock, err := wasmloop.LoadLock(wasmloop.Dir())
	if err != nil {
		return ""
	}
	if e, ok := lock.Get(name); ok {
		return e.Version
	}
	return ""
}

func (s *Server) buildRegistryView(e wasmloop.RegistryEntry, installed, active map[string]bool, baseURL string) registryEntryView {
	requires := e.Requires
	if requires == nil {
		requires = []string{}
	}
	v := registryEntryView{
		Name: e.Name, Kind: string(e.Kind), Version: e.Version,
		Description: e.Description, Author: e.Author, Requires: requires,
		ShipsWithEngine: e.ShipWithKARMAX,
		Installed:       installed[e.Name],
		Active:          active[e.Name],
		SourceURL:       loopregistry.GitHubSourceURL(baseURL, e.Source),
	}
	if e.Kind == wasmloop.KindWorkflow {
		v.InstalledVersion = installedWorkflowVersion(e.Name)
	}
	return v
}

// handleLoopsRegistryList serves GET /api/loops/registry — the marketplace:
// everything the public registry offers, cross-referenced against what is
// installed and what is actually running here. ?refresh=1 bypasses the
// 5-minute index cache.
func (s *Server) handleLoopsRegistryList(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	idx, fetchedAt, baseURL, _, err := s.registryCache.Get(r.Context(), refresh)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	installed := loopregistry.InstalledNames()
	active := s.activeLoopNames()
	views := make([]registryEntryView, 0, len(idx.Entries))
	for _, e := range idx.Entries {
		views = append(views, s.buildRegistryView(e, installed, active, baseURL))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":   views,
		"fetchedAt": fetchedAt.UTC().Format(time.RFC3339),
		"source":    baseURL + wasmloop.IndexFile,
	})
}

// handleLoopsRegistryDetail serves GET /api/loops/registry/{name} — enough to
// decide whether to install something without installing it first: the raw
// definition, its trigger, and what it can touch.
func (s *Server) handleLoopsRegistryDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !loopNamePattern.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid loop name"})
		return
	}

	idx, _, baseURL, _, err := s.registryCache.Get(r.Context(), false)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	e, ok := idx.Find(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such loop in the registry: " + name})
		return
	}
	view := s.buildRegistryView(e, loopregistry.InstalledNames(), s.activeLoopNames(), baseURL)

	client := wasmloop.NewClient()
	switch e.Kind {
	case wasmloop.KindRecipe:
		// The index's own sha256 is the only integrity check a recipe gets
		// (see wasmloop.Client.Fetch) — a mismatch here means never serve it,
		// exactly as install refuses to write it.
		data, err := client.Fetch(r.Context(), e)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		rec, err := loopregistry.ParseRecipeArtifact(e.Name, data)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		toolsUsed := recipes.NamedTools(rec)
		if toolsUsed == nil {
			toolsUsed = []string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"entry":      view,
			"definition": string(data),
			"trigger":    loopregistry.RecipeTrigger(rec),
			"tools":      toolsUsed,
			"host":       []string{},
		})
	case wasmloop.KindWorkflow:
		manifest, text, err := loopregistry.FetchWorkflowManifest(r.Context(), client, e.Source)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		toolsUsed, host := manifest.Tools, manifest.Host
		if toolsUsed == nil {
			toolsUsed = []string{}
		}
		if host == nil {
			host = []string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"entry":      view,
			"definition": text,
			"trigger":    loopregistry.WorkflowTrigger(manifest),
			"tools":      toolsUsed,
			"host":       host,
		})
	default:
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": fmt.Sprintf("%s is a %q, which this KARMAX does not know how to describe", e.Name, e.Kind)})
	}
}

// handleLoopsRegistryInstall serves POST /api/loops/registry/{name}/install —
// the app's replacement for `karmax loops install <name> --yes`. It runs
// exactly that verification; the only decision left to the caller is
// allowUntrusted, standing in for the CLI's confirmUnreviewed prompt.
func (s *Server) handleLoopsRegistryInstall(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !loopNamePattern.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid loop name"})
		return
	}
	var req struct {
		AllowUntrusted bool `json:"allowUntrusted"`
	}
	if r.Body != nil {
		// Best-effort: an empty body is a plain "install", same as the CLI's
		// bare `--yes` with no `--untrusted`.
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	client := wasmloop.NewClient()
	idx, err := client.Index(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	e, ok := idx.Find(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such loop in the registry: " + name})
		return
	}
	data, err := client.Fetch(ctx, e)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	switch e.Kind {
	case wasmloop.KindRecipe:
		if _, err := loopregistry.ParseRecipeArtifact(e.Name, data); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		path, replaced, err := loopregistry.WriteRecipe(e.Name, data)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		msg := fmt.Sprintf("Wrote %s. KARMAX picks it up without a restart.", path)
		if replaced {
			msg = fmt.Sprintf("Replaced the installed %s with %s. KARMAX picks it up without a restart.", e.Name, e.Version)
		}
		writeJSON(w, http.StatusOK, map[string]any{"installed": true, "restartRequired": false, "message": msg})

	case wasmloop.KindWorkflow:
		in := &wasmloop.Installer{
			Dir: wasmloop.Dir(), Broker: s.store,
			Trust: wasmloop.LoadTrust(wasmloop.Dir()), Actor: "api",
		}
		preview, alreadyCurrent, err := loopregistry.InstallWorkflow(in, data, req.AllowUntrusted)
		if err != nil {
			if errors.Is(err, loopregistry.ErrUntrusted) {
				writeJSON(w, http.StatusConflict, map[string]any{
					"error":     fmt.Sprintf("%s is not countersigned by a registry this instance trusts; retry with allowUntrusted to accept it anyway", e.Name),
					"untrusted": true,
				})
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		if alreadyCurrent {
			writeJSON(w, http.StatusOK, map[string]any{
				"installed": true, "restartRequired": false,
				"message": fmt.Sprintf("%s %s is already installed.", e.Name, preview.Manifest.Version),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"installed": true, "restartRequired": true,
			"message": fmt.Sprintf("Installed %s %s. Restart KARMAX to run it.", e.Name, preview.Manifest.Version),
		})

	default:
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": fmt.Sprintf("%s is a %q, which this KARMAX does not know how to install", e.Name, e.Kind)})
	}
}

// handleLoopsRegistryUninstall serves DELETE /api/loops/registry/{name}. It
// works entirely from local state (no registry fetch needed to find or
// remove a recipe, and only a best-effort one to check "ships with KARMAX"
// for a workflow) — an uninstall must not depend on the network being up.
func (s *Server) handleLoopsRegistryUninstall(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !loopNamePattern.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid loop name"})
		return
	}

	if _, err := os.Stat(filepath.Join(recipes.Dir(), name+".yaml")); err == nil {
		for _, b := range recipes.BuiltinNames() {
			if b == name {
				writeJSON(w, http.StatusConflict, map[string]any{"error": name + " ships with KARMAX and cannot be removed"})
				return
			}
		}
		if err := loopregistry.RemoveRecipe(name); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": true, "restartRequired": false})
		return
	}

	lock, lockErr := wasmloop.LoadLock(wasmloop.Dir())
	if lockErr == nil {
		if _, ok := lock.Get(name); ok {
			// Best-effort: if the registry can't be reached right now, an
			// uninstall must still work rather than depend on it — this check
			// only ever turns a removal INTO a 409, never blocks one outright.
			if idx, _, _, _, err := s.registryCache.Get(r.Context(), false); err == nil {
				if e, found := idx.Find(name); found && e.Kind == wasmloop.KindWorkflow && e.ShipWithKARMAX {
					writeJSON(w, http.StatusConflict, map[string]any{"error": name + " ships with KARMAX and cannot be removed"})
					return
				}
			}
			in := &wasmloop.Installer{Dir: wasmloop.Dir(), Broker: s.store}
			if err := in.Remove(name); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"removed": true, "restartRequired": true})
			return
		}
	}

	writeJSON(w, http.StatusNotFound, map[string]any{"error": "not installed: " + name})
}

// handleEnableLoop and handleDisableLoop serve POST /api/loops/{name}/enable
// and /disable — what `karmax loops enable|disable` does (loopinstall's
// operator-disabled list, which loophost.go's applyRecipes and
// startLoopkitLoops both consult, governing every tier from one file).
func (s *Server) handleEnableLoop(w http.ResponseWriter, r *http.Request) {
	s.setLoopEnabled(w, r, true)
}

func (s *Server) handleDisableLoop(w http.ResponseWriter, r *http.Request) {
	s.setLoopEnabled(w, r, false)
}

func (s *Server) setLoopEnabled(w http.ResponseWriter, r *http.Request, on bool) {
	name := r.PathValue("name")
	if !loopNamePattern.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid loop name"})
		return
	}
	if !s.loopExists(name) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such loop: " + name})
		return
	}
	if err := loopinstall.SetLoopDisabled(name, !on); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// A recipe is re-applied now; every other tier is only read at start, and
	// saying so is the difference between "paused" and a loop that fires anyway.
	restart := s.loopKind(name) != "recipe"
	if !restart && s.loopsChanged != nil {
		s.loopsChanged()
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "enabled": on, "restartRequired": restart})
}

// loopKind is the tier a loop runs in, from the live listing, or "recipe" for
// a recipe file the listing does not show (a disabled one, say).
func (s *Server) loopKind(name string) string {
	if s.listLoops != nil {
		for _, l := range s.listLoops() {
			if l.Name == name && l.Kind != "" {
				return l.Kind
			}
		}
	}
	if _, err := os.Stat(filepath.Join(recipes.Dir(), name+".yaml")); err == nil {
		return "recipe"
	}
	return "workflow"
}

// loopExists reports whether the daemon knows a loop by this name at all:
// running now, a recipe file on disk (whatever its own enabled: says), or a
// signed workflow in the lockfile. Without this check, enabling a name
// nothing has ever heard of would still "succeed" — it would just write an
// inert line into loops-disabled.txt's removal list and report enabled:true
// for something that was never going to run either way.
func (s *Server) loopExists(name string) bool {
	if s.listLoops != nil {
		for _, l := range s.listLoops() {
			if l.Name == name {
				return true
			}
		}
	}
	if _, err := os.Stat(filepath.Join(recipes.Dir(), name+".yaml")); err == nil {
		return true
	}
	if lock, err := wasmloop.LoadLock(wasmloop.Dir()); err == nil {
		if _, ok := lock.Get(name); ok {
			return true
		}
	}
	return false
}
