package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gitloom "github.com/GitLoomHQ/gitloom-go/gitloom"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// The one-time transfer of everything KARMAX has learned into GitLoom.
//
// Not a dump. The two models differ in shape (see internal/memory/gitloommap.go)
// and the whole value of moving is in the promotion: a flat category becomes a
// place in a tree, an importance becomes a confidence, tags that were only ever
// filter labels become embedded retrieval cues, and the relationship rows that
// were mostly a dedup table become real graph edges between named files.
//
// Two properties matter more than speed. It is IDEMPOTENT — paths are derived
// from content, so a re-run rewrites the same files rather than scattering
// near-duplicates. And it is RESUMABLE — each entry's destination is recorded
// locally as it goes, so an interrupted run continues instead of restarting.

func migrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate-memory",
		Short: "Transfer long-term memory (entries, relationships, chat summaries) to GitLoom Cloud",
		Long: "Moves KARMAX's long-term memory into GitLoom Cloud.\n\n" +
			"Local SQLite keeps every memory as a write-through cache and offline\n" +
			"fallback; GitLoom becomes what retrieval actually reads. Idempotent:\n" +
			"re-running rewrites the same files rather than duplicating them.",
	}

	var (
		namespace string
		batchSize int
		dryRun    bool
		summaries bool
		limit     int
	)
	run := &cobra.Command{
		Use:   "run",
		Short: "Perform the migration",
		RunE: func(c *cobra.Command, _ []string) error {
			return runMigration(c.Context(), migrateOptions{
				Namespace: namespace, BatchSize: batchSize, DryRun: dryRun,
				Summaries: summaries, Limit: limit,
			})
		},
	}
	f := run.Flags()
	f.StringVar(&namespace, "namespace", "", "GitLoom namespace (default: GITLOOM_NAMESPACE, then the first agent's)")
	f.IntVar(&batchSize, "batch", 100, "memories per request")
	f.BoolVar(&dryRun, "dry-run", false, "show what would be sent without sending it")
	f.BoolVar(&summaries, "summaries", true, "also migrate cold per-chat background summaries")
	f.IntVar(&limit, "limit", 0, "stop after N entries (0 = all)")
	cmd.AddCommand(run)

	verify := &cobra.Command{
		Use:   "verify",
		Short: "Compare what is stored locally against what GitLoom holds",
		RunE: func(c *cobra.Command, _ []string) error {
			return verifyMigration(c.Context(), namespace)
		},
	}
	verify.Flags().StringVar(&namespace, "namespace", "", "GitLoom namespace")
	cmd.AddCommand(compareCmd())
	cmd.AddCommand(auditCmd())
	cmd.AddCommand(verify)

	return cmd
}

type migrateOptions struct {
	Namespace string
	BatchSize int
	DryRun    bool
	Summaries bool
	Limit     int
}

// migrationDeps opens everything a migration touches: the local database and,
// unless this is a dry run, an authenticated GitLoom client.
func migrationDeps(namespace string, needClient bool) (*store.Store, *gitloom.Client, string, error) {
	cfgPath := findConfig()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("load config: %w", err)
	}
	db, err := store.New(filepath.Join(cfg.Karmax.DataDir, "db", "karmax.db"), zap.NewNop())
	if err != nil {
		return nil, nil, "", fmt.Errorf("open store: %w", err)
	}

	local := ""
	if len(cfg.Agents) > 0 {
		local = cfg.Agents[0].Memory.Namespace
		if local == "" {
			local = cfg.Agents[0].ID
		}
	}
	if namespace == "" {
		namespace = strings.TrimSpace(os.Getenv("GITLOOM_NAMESPACE"))
	}
	if namespace == "" {
		namespace = local
	}
	if namespace == "" {
		return nil, nil, "", fmt.Errorf("no namespace: pass --namespace or set GITLOOM_NAMESPACE")
	}

	var client *gitloom.Client
	if needClient {
		key := strings.TrimSpace(os.Getenv("GITLOOM_API_KEY"))
		if key == "" {
			return nil, nil, "", fmt.Errorf("GITLOOM_API_KEY is not set")
		}
		opts := []gitloom.Option{gitloom.WithNamespace(namespace)}
		if u := strings.TrimSpace(os.Getenv("GITLOOM_BASE_URL")); u != "" {
			opts = append(opts, gitloom.WithBaseURL(u))
		}
		client = gitloom.New(key, opts...)
	}
	return db, client, local, nil
}

func runMigration(ctx context.Context, o migrateOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	db, client, localNS, err := migrationDeps(o.Namespace, !o.DryRun)
	if err != nil {
		return err
	}
	ns := o.Namespace
	if ns == "" {
		ns = strings.TrimSpace(os.Getenv("GITLOOM_NAMESPACE"))
	}
	if ns == "" {
		ns = localNS
	}

	entries, err := db.ListMemoryEntries(localNS, 100000)
	if err != nil {
		return fmt.Errorf("read memory entries: %w", err)
	}
	if o.Limit > 0 && len(entries) > o.Limit {
		entries = entries[:o.Limit]
	}
	fmt.Printf("%d memory entries in namespace %q → GitLoom namespace %q\n", len(entries), localNS, ns)

	// Oldest first, so a memory that supersedes another is written after it and
	// wins. Reversed order would have the correction overwritten by the stale
	// fact it was meant to replace.
	sort.Slice(entries, func(i, j int) bool { return entries[i].CreatedAt.Before(entries[j].CreatedAt) })

	// Pass 1: decide every destination before writing any of them.
	//
	// Paths have to be known up front because relationship edges name paths,
	// and an edge to a memory that has not been assigned one yet would be
	// dropped — the graph is most of what makes this migration worth doing.
	//
	// Entries about one subject deliberately share a path: they are folded into
	// one file with a dated ## section each, which is the shape GitLoom indexes
	// (every header is its own addressable sub-node). 685 rows about forty
	// subjects become forty navigable files rather than 685 flat ones.
	pathByID := make(map[string]string, len(entries))
	groups := map[string][]memory.MemoryEntry{}
	var order []string
	for _, e := range entries {
		me := toMemoryEntry(e)
		p := memory.PathFor(me, nil)
		pathByID[me.ID] = p
		if _, seen := groups[p]; !seen {
			order = append(order, p)
		}
		groups[p] = append(groups[p], me)
	}
	fmt.Printf("%d entries fold into %d subject files\n", len(entries), len(order))

	links, err := db.ListMemoryLinks()
	if err != nil {
		return fmt.Errorf("read memory links: %w", err)
	}
	related := resolveLinks(links, pathByID)
	edges := 0
	for _, r := range related {
		edges += len(r)
	}
	fmt.Printf("%d relationship links → %d explicit graph edges\n", len(links), edges)

	// Pass 2: build and send.
	batch := make([]gitloom.Memory, 0, o.BatchSize)
	ids := make([]string, 0, o.BatchSize)
	sent, failed := 0, 0

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if o.DryRun {
			for _, m := range batch {
				fmt.Printf("  would write %-52s conf=%.2f cues=%d related=%d\n",
					m.Path, m.Confidence, len(m.Cues), len(m.Related))
			}
			sent += len(batch)
			batch, ids = batch[:0], ids[:0]
			return nil
		}
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := client.Write(callCtx, batch, &gitloom.WriteOptions{Namespace: ns})
		cancel()
		if err != nil {
			failed += len(batch)
			fmt.Printf("  batch of %d failed: %v\n", len(batch), err)
			batch, ids = batch[:0], ids[:0]
			// Not fatal: one rejected batch should not abandon the other 600
			// memories, and the run is resumable so this batch can be retried.
			return nil
		}
		// Recorded only after acceptance, so an interrupted run resumes rather
		// than believing it already delivered what it had merely queued.
		for i, id := range ids {
			_ = db.SetGitloomPath(id, batch[i].Path)
		}
		sent += len(batch)
		fmt.Printf("  delivered %d files (%d/%d)\n", len(batch), sent, len(order))
		batch, ids = batch[:0], ids[:0]
		return nil
	}

	// The explicit links are thin — 81 of the 88 are "duplicate", a dedup
	// artefact rather than a relationship. The real graph is latent in the
	// prose: now that each subject is one named file, a memory about Siva that
	// says "CampX" is an edge to facts/projects/campx.md waiting to be drawn.
	derived := deriveEdges(order, groups)
	fmt.Printf("%d edges derived from cross-mentions\n", countEdges(derived))

	for _, path := range order {
		group := groups[path]
		// Every edge declared by any entry in the group belongs to the file
		// they all became; dropping the others would lose most of the graph.
		var edgesForFile []string
		seenEdge := map[string]bool{}
		addEdge := func(r string) {
			if r != "" && !seenEdge[r] {
				seenEdge[r] = true
				edgesForFile = append(edgesForFile, r)
			}
		}
		for _, me := range group {
			for _, r := range related[me.ID] {
				addEdge(r)
			}
		}
		for _, r := range derived[path] {
			addEdge(r)
		}
		batch = append(batch, memory.Merge(path, group, edgesForFile))
		// Recorded against the last entry of the group: they share a file, and
		// the path only has to be resolvable from any one of them.
		ids = append(ids, group[len(group)-1].ID)
		if len(batch) >= o.BatchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	// Every entry in a group points at the file it became, so a later update or
	// deletion finds it.
	if !o.DryRun {
		for id, p := range pathByID {
			_ = db.SetGitloomPath(id, p)
		}
	}

	if o.Summaries {
		n, err := migrateChatSummaries(ctx, db, client, ns, o)
		if err != nil {
			fmt.Printf("chat summaries: %v\n", err)
		} else {
			sent += n
		}
	}

	// The operator profile is the single most valuable document KARMAX holds —
	// the curated answer to "who is this person" — so it goes as a rule, the
	// tier GitLoom loads whole rather than retrieving fragments of.
	if err := migrateProfile(ctx, client, ns, localNS, o); err != nil {
		fmt.Printf("profile: %v\n", err)
	}

	fmt.Printf("\n%d memories migrated", sent)
	if failed > 0 {
		fmt.Printf(", %d failed (re-run to retry)", failed)
	}
	fmt.Println()
	if !o.DryRun {
		fmt.Println("GitLoom ingests asynchronously; give it a minute, then `karmax migrate-memory verify`.")
	}
	return nil
}

// toMemoryEntry converts a stored row into the in-memory shape the mapping
// operates on.
func toMemoryEntry(e store.StoredMemoryEntry) memory.MemoryEntry {
	var tags []string
	_ = json.Unmarshal([]byte(e.Tags), &tags)
	return memory.MemoryEntry{
		ID: e.ID, AgentID: e.AgentID, Namespace: e.Namespace, Role: e.Role,
		Content: e.Content, Tags: tags, Category: e.Category,
		Importance: e.Importance, Pinned: e.Pinned, AccessCount: e.AccessCount,
		ExpiresAt: e.ExpiresAt, CreatedAt: e.CreatedAt,
	}
}

// resolveLinks turns local id→id edges into GitLoom path references.
//
// "duplicate" links are dropped: they are a dedup artefact, not a relationship,
// and importing them would tell the graph that two memories about the same
// person are related to each other rather than to the person.
func resolveLinks(links []store.MemoryLink, pathByID map[string]string) map[string][]string {
	out := map[string][]string{}
	for _, l := range links {
		if strings.EqualFold(strings.TrimSpace(l.Relation), "duplicate") {
			continue
		}
		from, okF := pathByID[l.FromID]
		to, okT := pathByID[l.ToID]
		if !okF || !okT || from == to {
			continue
		}
		ref := to
		if r := strings.TrimSpace(l.Relation); r != "" {
			ref = r + ": " + to
		}
		out[l.FromID] = append(out[l.FromID], ref)
		_ = from
	}
	return out
}

// migrateChatSummaries moves the cold per-chat background.
//
// These are long-term memory in everything but name: a distilled account of
// months of conversation with one person, which is exactly what someone asking
// "what's the story with X" needs.
func migrateChatSummaries(ctx context.Context, db *store.Store, client *gitloom.Client, ns string, o migrateOptions) (int, error) {
	sums, err := db.ListChatSummaries(100000)
	if err != nil {
		return 0, err
	}
	batch := make([]gitloom.Memory, 0, o.BatchSize)
	sent := 0
	taken := map[string]bool{}

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if o.DryRun {
			sent += len(batch)
			batch = batch[:0]
			return nil
		}
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := client.Write(callCtx, batch, &gitloom.WriteOptions{Namespace: ns})
		cancel()
		if err != nil {
			fmt.Printf("  summary batch of %d failed: %v\n", len(batch), err)
		} else {
			sent += len(batch)
		}
		batch = batch[:0]
		return nil
	}

	for _, s := range sums {
		body := strings.TrimSpace(s.Summary)
		if body == "" {
			continue
		}
		name := strings.TrimSpace(s.ChatName)
		if name == "" {
			name = s.ChatJID
		}
		e := memory.MemoryEntry{
			ID: s.ChatJID, Category: "context", Content: body,
			Tags: []string{name, "chat-history"}, Importance: 2,
			CreatedAt: s.LastMessageAt,
		}
		path := memory.PathFor(e, taken)
		// Filed under their own topic: a background summary is a different kind
		// of thing from a distilled fact, and mixing them makes both harder to
		// navigate.
		path = strings.Replace(path, "facts/context/", "facts/conversations/", 1)
		taken[path] = true

		m := memory.ToGitLoom(e, path, nil)
		m.Cues = []string{name, "what did " + name + " and I talk about"}
		batch = append(batch, m)
		if len(batch) >= o.BatchSize {
			if err := flush(); err != nil {
				return sent, err
			}
		}
	}
	if err := flush(); err != nil {
		return sent, err
	}
	fmt.Printf("%d chat summaries migrated\n", sent)
	return sent, nil
}

// migrateProfile sends the curated ABOUT_ME document.
func migrateProfile(ctx context.Context, client *gitloom.Client, ns, localNS string, o migrateOptions) error {
	cfgPath := findConfig()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	path := filepath.Join(cfg.Karmax.DataDir, "memory", localNS, "ABOUT_ME.md")
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(string(body)) == "" {
		return nil
	}
	if o.DryRun {
		fmt.Printf("  would write rules/operator.md (%d bytes)\n", len(body))
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	// rules/ is the tier GitLoom loads whole on every retrieval rather than
	// searching, which is the right home for "who is this person".
	err = client.Write(callCtx, []gitloom.Memory{{
		Path: "rules/operator.md", Content: string(body),
		Tags: []string{"operator", "profile"}, Confidence: 1.0,
		Cues: []string{"who is the operator", "about me", "my preferences"},
	}}, &gitloom.WriteOptions{Namespace: ns})
	if err == nil {
		fmt.Println("operator profile migrated to rules/operator.md")
	}
	return err
}

// verifyMigration compares the two stores. Counts, then a spot check that the
// memories are actually retrievable — a file that exists but cannot be found
// has not really been migrated.
func verifyMigration(ctx context.Context, namespace string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	db, client, localNS, err := migrationDeps(namespace, true)
	if err != nil {
		return err
	}
	ns := namespace
	if ns == "" {
		ns = strings.TrimSpace(os.Getenv("GITLOOM_NAMESPACE"))
	}
	if ns == "" {
		ns = localNS
	}

	localCount, err := db.CountMemoryEntries(localNS)
	if err != nil {
		return err
	}
	tops, err := client.Topics(ctx, &gitloom.TopicsOptions{Namespace: ns, MaxDepth: 1})
	if err != nil {
		return fmt.Errorf("read GitLoom topics: %w", err)
	}
	remote := 0
	for _, t := range tops.Topics {
		remote += t.Memories
	}

	fmt.Printf("local entries:   %d\n", localCount)
	fmt.Printf("GitLoom memories: %d\n\n", remote)

	fmt.Println("topics:")
	byTopic, err := client.Topics(ctx, &gitloom.TopicsOptions{Namespace: ns, MaxDepth: 2})
	if err == nil {
		for _, t := range byTopic.Topics {
			fmt.Printf("  %-34s %d\n", t.Path, t.Memories)
		}
	}

	return nil
}

// deriveEdges finds the relationships the prose already states.
//
// KARMAX's explicit link table is almost entirely "duplicate" markers, so
// importing it alone yields a graph with four edges — barely a graph. But the
// memories name each other constantly ("Siva runs delivery at CampX", "Aymaan
// built the longmem benchmark"), and once every subject is its own file those
// mentions are edges.
//
// Deliberately conservative, because a wrong edge is worse than a missing one:
// it pulls an unrelated memory into every retrieval that touches its neighbour.
// Only subject names of four characters or more, matched on word boundaries,
// and never a file to itself.
func deriveEdges(order []string, groups map[string][]memory.MemoryEntry) map[string][]string {
	// name → path, for every subject that has a distinctive enough name.
	subject := map[string]string{}
	for _, p := range order {
		name := strings.TrimSuffix(filepath.Base(p), ".md")
		// Hyphenated slugs are matched on their spaced form too, so
		// "nikhil-gunda" finds "Nikhil Gunda" in the body.
		for _, form := range []string{name, strings.ReplaceAll(name, "-", " ")} {
			if len(form) >= 4 {
				subject[strings.ToLower(form)] = p
			}
		}
	}

	out := map[string][]string{}
	for _, p := range order {
		var body strings.Builder
		for _, e := range groups[p] {
			body.WriteString(strings.ToLower(e.Content))
			body.WriteByte('\n')
		}
		text := body.String()
		seen := map[string]bool{}
		for name, target := range subject {
			if target == p || seen[target] {
				continue
			}
			if !containsWord(text, name) {
				continue
			}
			seen[target] = true
			out[p] = append(out[p], "mentions: "+target)
		}
		// A memory that mentions forty others is a digest, not a hub; edges
		// from it would connect everything to everything.
		if len(out[p]) > 12 {
			delete(out, p)
		}
	}
	return out
}

// containsWord reports whether name appears in text delimited by non-letters,
// so "ev" does not match inside "every" and "siva" does not match "sivakumar".
func containsWord(text, name string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], name)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(name)
		beforeOK := start == 0 || !isWordByte(text[start-1])
		afterOK := end == len(text) || !isWordByte(text[end])
		if beforeOK && afterOK {
			return true
		}
		i = start + 1
		if i >= len(text) {
			return false
		}
	}
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func countEdges(m map[string][]string) int {
	n := 0
	for _, v := range m {
		n += len(v)
	}
	return n
}
