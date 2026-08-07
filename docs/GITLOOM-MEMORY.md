# Offloading KARMAX's memory layer to GitLoom Cloud

## Why

KARMAX's memory today is a SQLite table plus a hand-rolled retrieval function.
`memory_entries` holds 684 rows; `SearchSemantic` extracts keywords from the
query, runs a `LIKE` scan per keyword, counts how many keywords each row
matched, and adds a boost for importance/pinned/recency/access-count. There are
no embeddings, no BM25, no provenance, and no way to ask *why* a memory ranked.
The relationship graph is 88 rows in `memory_links`, most of them
`duplicate` — it is a dedup table wearing a graph's name.

GitLoom is a memory engine: git-backed markdown, SQLite+FTS5 with BM25, hybrid
lexical+vector retrieval with reciprocal-rank fusion, a real relationship graph
with validity windows, PageIndex-style tree navigation, and per-result
provenance from git history. It scores 91.4% on LongMemEval. KARMAX is the
exact workload it was built for.

So: the memory layer moves to GitLoom Cloud. SQLite stays for everything that
is not memory — contacts, logs, events, channel messages, proposals,
notifications, scheduled jobs, and the short-term KV store.

## What stays local, and why

| Table | Rows | Destination | Why |
|---|---:|---|---|
| `memory_entries` | 684 | **GitLoom** | This is the memory layer. |
| `memory_links` | 88 | **GitLoom** | Relationship edges, first-class in GitLoom. |
| `chat_summaries` | 952 | **GitLoom** | Cold per-chat background = long-term memory. |
| `pageindex_nodes/trees` | 696 | dropped | A derived index GitLoom rebuilds better. |
| `contacts` | 918 | local | Operational lookup on the hot path of every message. |
| `channel_messages` | 1,704 | local | Transport log, not memory. Feeds ingestion. |
| `events` | 29,177 | local | Bus telemetry. Volume without meaning. |
| `notifications`, `proposals`, `reviews` | 612 | local | UI state with a lifecycle. |
| `kv_memory` | 5 | local | Short-term TTL store; deliberately ephemeral. |
| `coding_sessions`, `device_actions`, `push_tokens`, `scheduled_jobs` | — | local | Operational. |

The rule: **if it is a fact worth recalling later, it goes to GitLoom. If it is
machinery, it stays in SQLite.**

## The gaps in GitLoom Cloud, and how they are filled

GitLoom's cloud API was built around one write path: `POST /v1/memories` takes
a *conversation* and an LLM extracts memories from it. That is the right
primitive for a chat product, and it is the only one there is. Four things
KARMAX needs were missing — each of them general, none of them KARMAX-shaped:

1. **A direct write.** Not every memory comes from a conversation. KARMAX has
   684 already-distilled facts; pushing them through an LLM extractor would be
   lossy, slow and expensive, and would discard the importance, category and
   relationship metadata that KARMAX spent months accumulating. Anyone
   migrating from another memory store hits this same wall. The engine has had
   the operation all along (`toolkit.remember`, `gitloom write` on the CLI) —
   the cloud simply never exposed it.

2. **Delete.** The engine has no `Forget`. The only way a memory could leave
   the store was TTL expiry on the incidents tier. "Forget what you know about
   me" is a baseline requirement for a memory product, not a KARMAX feature.

3. **Read by path**, so a caller can fetch the memory a retrieval result
   pointed at.

4. **Tree and topic navigation.** The engine's PageIndex tree is one of its
   best ideas and the cloud exposed no way to walk it — only `/v1/retrieve`,
   which is search. KARMAX's `tree_navigate` tool maps onto it exactly.

All four are additions to the public product in its own idiom, on the branch
`feat/direct-memory-api`.

### Direct writes go through the same queue

The tempting implementation is a synchronous write in the API Lambda. That is
wrong: it opens a second writer per namespace, racing the ingest worker for the
same S3 object, and the CAS would start rejecting one of them under normal use.

Instead a direct write is a **different kind of job on the same FIFO queue**,
with the namespace as the message group — exactly as ingestion already is. It
inherits the per-namespace serialization, the ETag compare-and-swap, the
registry commit and the quota accounting, and it costs no model calls. Because
one job carries many memories, migrating 684 of them is a handful of git
commits and S3 round trips rather than 684 of them.

## KARMAX's side

`internal/memory` grows a `Store` interface that today's `Manager` already
satisfies, plus a GitLoom-backed implementation. Call sites do not change.

    memory.Store
      ├── local   — SQLite + markdown (today; still the default for
      │             self-hosted users with no GitLoom account)
      └── gitloom — GitLoom Cloud via the Go SDK

Writes go through a **local outbox** first. KARMAX is a daemon on a laptop or a
Pi behind home internet; a memory must not be lost because the network blinked.
The outbox is a SQLite table, drained by a flusher, and it is what makes the
cloud dependency safe to take.

Retrieval prefers GitLoom and falls back to the local store when the API is
unreachable, so a network outage degrades quality instead of removing memory.

## What the migration actually did

684 memory entries did not become 684 files. They fold into **197 subject
files**, because two memories about CampX belong in one file as separate
dated `##` sections — GitLoom indexes every header as its own addressable
sub-node, so that shape gives retrieval something to point *at* and navigation
a tree with real branches. 685 flat files would have reproduced the pile the
migration existed to leave.

    facts/projects      49      incidents/2026/07   37
    facts/people        42      incidents/2026/08   14
    facts/conversations 69      facts/operator      12
    facts/context       34      facts/decisions      7
    rules/operator.md    1      facts/reference      1

The relationship graph is the part that changed most. KARMAX's 88 `memory_links`
are almost entirely `duplicate` markers — a dedup table, not a graph — and
importing them yields **4 edges**. But the memories name each other constantly,
and once every subject is its own file those mentions are edges: cross-mention
detection derives **722**.

## Comparison

Ten questions phrased the way the operator would ask them, not the way the
memories are written — which is the case keyword search cannot serve. Scored on
whether the right memory is in the **top 3**, because that is what an agent
composing a reply actually reads.

| | local SQLite | GitLoom |
|---|---:|---:|
| right memory in the top 3 | **3/10** | **6/10** |
| mean latency | 8.5 ms | 239 ms |

Latency is not the trade: an in-process `LIKE` scan will always beat a network
call. What is bought is the first row. Reproduce with
`karmax migrate-memory compare --verbose`.

**A finding worth keeping.** GitLoom's retrieval defaulted to attaching git
provenance to every hit, which costs a `git log` walk *per hit*. On this
namespace that was the entire request: **3.29 s with provenance, 0.40 s
without**, while dropping relations changed nothing. The SDK had no way to send
the opt-out the endpoint already accepted, so every SDK caller paid for
citations whether or not they showed any. KARMAX now asks for relations but not
provenance.

## Phases

0. Branches, namespace and API key. ✅
1. GitLoom engine + cloud API additions; deployed to dev and prod. ✅
2. Go SDK: the new primitives + the provenance opt-out. ✅
3. KARMAX: GitLoom backend, outbox, namespace routing, config. ✅
4. Migration: 684 entries + links + 69 chat summaries + the profile. ✅
5. Comparison. ✅
