package memory

import (
	"context"
	"strings"
	"sync"
	"time"

	gitloom "github.com/GitLoomHQ/gitloom-go/gitloom"
)

// Writes are asynchronous server-side, so a read right after a write can still
// return the old file or none. KARMAX keeps what it last did to each path —
// the body it wrote, or what it forgot — and folds onto that whenever the
// server has not caught up.
//
// How long that is trusted is the store's to show, not a clock's. It was a
// fixed two minutes, and the live store takes about two minutes to ingest a
// write (embedding alone is about two), so a read just after them found the
// store still without the write and the next fact folded onto that copy and
// replaced it. Now a change is trusted until a read shows the store holding it,
// and for pendingSettle after: the store's readers serve snapshots of
// different ages, so the one reader that has it says nothing of the next.
// pendingCap bounds a change the store is never seen to hold.
//
// Whether a copy holds the change is read from the copy, not from its
// updated_at. That is when the store applied a write, and writes queue: a
// write sent before this one and applied after it carries a later time and
// none of this one's content.

const (
	// pendingSettle is how long a change the store has been seen to hold is
	// still trusted, while its other readers catch up. They recheck the
	// store's head every 30 seconds and then fetch it; one was seen 30
	// seconds and more behind.
	pendingSettle = 5 * time.Minute
	// pendingCap is how long a change the store has never been seen to hold
	// is trusted over it: thirty times an ingest, for a queue of writes.
	pendingCap = time.Hour
)

type pendingWrite struct {
	stored gitloom.StoredMemory
	at     time.Time
	// seq tells one record from the one that replaced it.
	seq uint64
	// seen is when a read first showed the store holding this change.
	seen time.Time
	// gone is a document KARMAX forgot: absent, whatever an older copy says.
	gone bool
	// cut are the headers of sections KARMAX forgot from the document, cut
	// again from any copy that still holds them.
	cut []string
	// handles are the store's handles for those sections, by header.
	handles map[string]string
}

// expired reports a record the store's word has replaced.
func (w pendingWrite) expired(now time.Time) bool {
	if !w.seen.IsZero() {
		return now.Sub(w.seen) > pendingSettle
	}
	return now.Sub(w.at) > pendingCap
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

// record files a change to path; called with pmu held.
func (g *gitloomBackend) record(path string, w pendingWrite) {
	now := g.now()
	for p, old := range g.pending {
		if old.expired(now) {
			delete(g.pending, p)
		}
	}
	g.pseq++
	w.seq = g.pseq
	g.pending[path] = w
}

// remember records a body that was just sent, dated by when sending began.
func (g *gitloomBackend) remember(m gitloom.NewMemory, occurred, sentAt time.Time) {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	// A section forgotten a moment ago is still cut from older copies after
	// the document is written again — unless this write says it again. A fact
	// said again after it was forgotten is filed under the same header, and
	// cutting it from later reads lost it at the next change to the document.
	var cut []string
	handles := map[string]string{}
	if prev, ok := g.pending[m.Path]; ok && !prev.expired(g.now()) {
		written := map[string]bool{}
		for _, h := range sectionHeaders(m.Content) {
			written[h] = true
		}
		for _, h := range prev.cut {
			if !written[strings.TrimSpace(h)] {
				cut = append(cut, h)
				if handle, ok := prev.handles[h]; ok {
					handles[h] = handle
				}
			}
		}
	}
	g.record(m.Path, pendingWrite{at: sentAt, cut: cut, handles: handles, stored: gitloom.StoredMemory{
		Path: m.Path, Content: m.Content, UserTags: m.Tags, Cues: m.Cues, Related: m.Related,
		Confidence: m.Confidence, OccurredAt: occurred, UpdatedAt: sentAt,
		// The expiry it was written with, so a fold onto this copy keeps it.
		ExpiresAt: ttlExpiry(m.TTL, sentAt),
	}})
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

// holdsSection reports a document that still has any of these sections.
func holdsSection(doc string, headers []string) bool {
	want := map[string]bool{}
	for _, h := range headers {
		want[strings.TrimSpace(h)] = true
	}
	for _, h := range sectionHeaders(doc) {
		if want[h] {
			return true
		}
	}
	return false
}

// forgotten records a forget, so the reads that follow agree with it.
//
// The store's readers do not all see a change at once: one can answer that a
// forgotten fact is gone while another still serves the document from before,
// and the next write to the document folds onto whichever it reads. Without
// this a forget was undone by the very next write. base is the document as it
// stood (nil for a whole document); header names the section cut from it, and
// handle is the store's handle for that section.
func (g *gitloomBackend) forgotten(path string, base *gitloom.StoredMemory, header, handle string) {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	now := g.now()
	if header == "" {
		g.record(path, pendingWrite{at: now, gone: true})
		return
	}
	w := pendingWrite{at: now, cut: []string{header}, handles: map[string]string{header: handle}}
	if prev, ok := g.pending[path]; ok && !prev.gone && !prev.expired(now) {
		w.cut = append(append([]string(nil), prev.cut...), header)
		for h, ph := range prev.handles {
			w.handles[h] = ph
		}
	}
	if base != nil {
		w.stored = *base
		w.stored.Content = cutSections(base.Content, w.cut)
		w.stored.UpdatedAt = w.at
	}
	g.record(path, w)
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
	if ok && w.expired(g.now()) {
		delete(g.pending, path)
		return pendingWrite{}, false
	}
	return w, ok
}

// alreadyForgotten reports a document, or a section of one, that KARMAX has
// forgotten and the store may not have caught up with.
func (g *gitloomBackend) alreadyForgotten(handle string) bool {
	file, _, isSection := strings.Cut(handle, "#")
	w, ok := g.pendingFor(file)
	if !ok {
		return false
	}
	if w.gone {
		return true
	}
	if isSection {
		for _, h := range w.handles {
			if h == handle {
				return true
			}
		}
	}
	return false
}

// asKnown is a memory the store returned, as KARMAX knows it: false for one
// it has forgotten, and otherwise the text with any section it forgot cut out.
//
// A forget returns once the store has accepted it, and the store's readers
// take minutes to show it, so without this a search straight after "forgot
// it" recalled it, and so did any reader still serving the copy from before.
// updated is when the store last wrote the memory (zero when unknown): a
// forgotten document someone has written since is no longer forgotten.
func (g *gitloomBackend) asKnown(path, content string, updated time.Time) (string, bool) {
	file, _, _ := strings.Cut(path, "#")
	w, ok := g.pendingFor(file)
	if !ok {
		return content, true
	}
	if w.gone {
		return content, !updated.IsZero() && updated.After(w.at)
	}
	for _, h := range w.handles {
		if h == path {
			return "", false
		}
	}
	if len(w.cut) == 0 {
		return content, true
	}
	cut := cutSections(content, w.cut)
	if strings.TrimSpace(cut) == "" && strings.TrimSpace(content) != "" {
		return "", false
	}
	return cut, true
}

// sawHeld notes that a read showed the store holding w, unless w has been
// replaced since.
func (g *gitloomBackend) sawHeld(path string, w pendingWrite) {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	cur, ok := g.pending[path]
	if !ok || cur.seq != w.seq || !cur.seen.IsZero() {
		return
	}
	cur.seen = g.now()
	g.pending[path] = cur
}

// current is Get, reconciled with what KARMAX last did there. See reconcile.
func (g *gitloomBackend) current(ctx context.Context, path string) (*gitloom.StoredMemory, error) {
	existing, err := g.client.Get(ctx, path, &gitloom.RecallOptions{Namespace: g.cfg.Namespace})
	w, ok := g.pendingFor(path)
	if !ok {
		return existing, err
	}
	got, holds, err := reconcile(w, existing, err)
	if holds {
		g.sawHeld(path, w)
	}
	return got, err
}

// reconcile is the store's copy of a document (or its error) reconciled with
// what KARMAX last did to it, and whether that copy already holds all of it.
//
// By content first: a server copy holding the local body is caught up, and
// one the local body extends is behind. Only a copy that is neither (another
// writer, or a rewrite) is settled by which was updated last. A section
// KARMAX forgot is cut from any copy that still has it, and a document it
// forgot is absent unless someone has written it since.
func reconcile(w pendingWrite, existing *gitloom.StoredMemory, err error) (got *gitloom.StoredMemory, holds bool, _ error) {
	if w.gone {
		if err == nil && existing != nil && updatedAt(existing).After(w.at) {
			return existing, false, nil
		}
		return nil, isAbsent(err), &gitloom.APIError{Status: 404, Code: "not_found", Message: "forgotten"}
	}
	held := (err == nil && existing != nil) || isAbsent(err)
	if err == nil && existing != nil && len(w.cut) > 0 {
		// A copy that still has a forgotten section is from before the forget.
		held = !holdsSection(existing.Content, w.cut)
		c := *existing
		c.Content = cutSections(existing.Content, w.cut)
		existing = &c
	}
	if strings.TrimSpace(w.stored.Content) == "" {
		// Only a cut is known, not the document: the server's copy, cut.
		return existing, held, err
	}
	if err != nil {
		if !isAbsent(err) {
			return nil, false, err
		}
	} else if existing != nil {
		have, mine := strings.TrimSpace(existing.Content), strings.TrimSpace(w.stored.Content)
		switch {
		case strings.Contains(have, mine):
			return existing, held, nil
		case strings.Contains(mine, have):
		default:
			if updatedAt(existing).After(w.at) {
				return existing, false, nil
			}
		}
	}
	local := w.stored
	return &local, false, nil
}

// updatedAt is when the store last applied a write to a document.
func updatedAt(m *gitloom.StoredMemory) time.Time {
	return firstTime(stamp(m.UpdatedAt, m.Updated), stamp(m.CreatedAt, m.Created))
}
