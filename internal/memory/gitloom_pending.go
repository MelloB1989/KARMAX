package memory

import (
	"context"
	"strings"
	"sync"
	"time"

	gitloom "github.com/GitLoomHQ/gitloom-go/gitloom"
)

// Writes are asynchronous server-side, so a read right after a write can still
// return the old file or none. KARMAX keeps what it last wrote per path for a
// short while and folds onto that whenever the server has not caught up.

// pendingTTL bounds how long a written body is trusted over the server's.
const pendingTTL = 2 * time.Minute

type pendingWrite struct {
	stored gitloom.StoredMemory
	at     time.Time
	// gone is a document KARMAX forgot: absent, whatever an older copy says.
	gone bool
	// cut are the headers of sections KARMAX forgot from the document, cut
	// again from any copy that still holds them.
	cut []string
}

// lockPath serialises read-modify-write on one path.
func (g *gitloomBackend) lockPath(path string) func() {
	g.pmu.Lock()
	l, ok := g.pathLocks[path]
	if !ok {
		l = &sync.Mutex{}
		g.pathLocks[path] = l
	}
	g.pmu.Unlock()
	l.Lock()
	return l.Unlock
}

// remember records a body that was just sent, dated by when sending began.
func (g *gitloomBackend) remember(m gitloom.NewMemory, occurred, sentAt time.Time) {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	for p, w := range g.pending {
		if time.Since(w.at) > pendingTTL {
			delete(g.pending, p)
		}
	}
	// A section forgotten a moment ago is still cut from older copies after
	// the document is written again — unless this write says it again. A fact
	// said again after it was forgotten is filed under the same header, and
	// cutting it from later reads lost it at the next change to the document.
	var cut []string
	if prev, ok := g.pending[m.Path]; ok {
		written := map[string]bool{}
		for _, h := range sectionHeaders(m.Content) {
			written[h] = true
		}
		for _, h := range prev.cut {
			if !written[strings.TrimSpace(h)] {
				cut = append(cut, h)
			}
		}
	}
	g.pending[m.Path] = pendingWrite{at: sentAt, cut: cut, stored: gitloom.StoredMemory{
		Path: m.Path, Content: m.Content, UserTags: m.Tags, Cues: m.Cues, Related: m.Related,
		Confidence: m.Confidence, OccurredAt: occurred, UpdatedAt: sentAt,
		// The expiry it was written with, so a fold onto this copy keeps it.
		ExpiresAt: ttlExpiry(m.TTL, sentAt),
	}}
}

// sectionHeaders are a document's "## " headers, trimmed.
func sectionHeaders(doc string) []string {
	var out []string
	for _, l := range strings.Split(doc, "\n") {
		if strings.HasPrefix(l, "## ") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(l, "## ")))
		}
	}
	return out
}

// forgotten records a forget, so the reads that follow agree with it.
//
// The store's readers do not all see a change at once: one can answer that a
// forgotten fact is gone while another still serves the document from before,
// and the next write to the document folds onto whichever it reads. Without
// this a forget was undone by the very next write. base is the document as it
// stood (nil for a whole document); header names the section cut from it.
func (g *gitloomBackend) forgotten(path string, base *gitloom.StoredMemory, header string) {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	if header == "" {
		g.pending[path] = pendingWrite{at: time.Now(), gone: true}
		return
	}
	w := pendingWrite{at: time.Now(), cut: []string{header}}
	if prev, ok := g.pending[path]; ok && !prev.gone {
		w.cut = append(prev.cut, header)
	}
	if base != nil {
		w.stored = *base
		w.stored.Content = cutSections(base.Content, w.cut)
		w.stored.UpdatedAt = w.at
	}
	g.pending[path] = w
}

// cutSections removes the "## " sections with these headers from a document.
func cutSections(doc string, headers []string) string {
	if len(headers) == 0 {
		return doc
	}
	drop := map[string]bool{}
	for _, h := range headers {
		drop[strings.TrimSpace(h)] = true
	}
	var kept []string
	skipping := false
	for _, l := range strings.Split(doc, "\n") {
		if strings.HasPrefix(l, "## ") {
			skipping = drop[strings.TrimSpace(strings.TrimPrefix(l, "## "))]
		}
		if !skipping {
			kept = append(kept, l)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func (g *gitloomBackend) dropPending(path string) {
	g.pmu.Lock()
	delete(g.pending, path)
	g.pmu.Unlock()
}

func (g *gitloomBackend) pendingFor(path string) (pendingWrite, bool) {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	w, ok := g.pending[path]
	if ok && time.Since(w.at) > pendingTTL {
		delete(g.pending, path)
		return pendingWrite{}, false
	}
	return w, ok
}

// current is Get, reconciled with what KARMAX last wrote there. By content
// first: a server copy holding the local body is caught up, and one the local
// body extends is behind. Only a copy that is neither (another writer, or a
// rewrite) is settled by which was updated last.
func (g *gitloomBackend) current(ctx context.Context, path string) (*gitloom.StoredMemory, error) {
	existing, err := g.client.Get(ctx, path, &gitloom.RecallOptions{Namespace: g.cfg.Namespace})
	w, ok := g.pendingFor(path)
	if !ok {
		return existing, err
	}
	if w.gone {
		// Forgotten: absent, unless someone has written it since.
		if err == nil && existing != nil && firstTime(stamp(existing.UpdatedAt, existing.Updated)).After(w.at) {
			return existing, nil
		}
		return nil, &gitloom.APIError{Status: 404, Code: "not_found", Message: "forgotten"}
	}
	if err == nil && existing != nil && len(w.cut) > 0 {
		c := *existing
		c.Content = cutSections(existing.Content, w.cut)
		existing = &c
	}
	if strings.TrimSpace(w.stored.Content) == "" {
		// Only a cut is known, not the document: the server's copy, cut.
		return existing, err
	}
	if err != nil {
		if !isAbsent(err) {
			return nil, err
		}
	} else if existing != nil {
		have, mine := strings.TrimSpace(existing.Content), strings.TrimSpace(w.stored.Content)
		switch {
		case strings.Contains(have, mine):
			return existing, nil
		case strings.Contains(mine, have):
		default:
			updated := firstTime(stamp(existing.UpdatedAt, existing.Updated), stamp(existing.CreatedAt, existing.Created))
			if updated.After(w.at) {
				return existing, nil
			}
		}
	}
	local := w.stored
	return &local, nil
}
