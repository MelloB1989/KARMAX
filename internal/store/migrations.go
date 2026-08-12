package store

import "strings"

var migrations = []string{
	// 001_agents
	`CREATE TABLE IF NOT EXISTS agent_snapshots (
		id          TEXT PRIMARY KEY,
		name        TEXT NOT NULL,
		status      TEXT NOT NULL,
		restarts    INTEGER DEFAULT 0,
		started_at  DATETIME,
		last_event  DATETIME,
		last_err    TEXT,
		def_json    TEXT NOT NULL,
		state_json  TEXT,
		updated_at  DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,

	// 002_events
	`CREATE TABLE IF NOT EXISTS events (
		id          TEXT PRIMARY KEY,
		kind        TEXT NOT NULL,
		agent_id    TEXT,
		payload     TEXT NOT NULL,
		meta        TEXT,
		created_at  DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_events_agent ON events(agent_id)`,
	`CREATE INDEX IF NOT EXISTS idx_events_kind  ON events(kind)`,
	`CREATE INDEX IF NOT EXISTS idx_events_time  ON events(created_at)`,

	// 003_memory
	`CREATE TABLE IF NOT EXISTS memory_entries (
		id          TEXT PRIMARY KEY,
		agent_id    TEXT NOT NULL,
		namespace   TEXT NOT NULL,
		role        TEXT NOT NULL,
		content     TEXT NOT NULL,
		tags        TEXT,
		created_at  DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_mem_ns ON memory_entries(namespace)`,

	// 004_pageindex
	`CREATE TABLE IF NOT EXISTS pageindex_trees (
		namespace   TEXT PRIMARY KEY,
		tree_blob   TEXT NOT NULL,
		toc_blob    TEXT,
		updated_at  DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,

	// 005_scheduler
	`CREATE TABLE IF NOT EXISTS scheduled_jobs (
		id          TEXT PRIMARY KEY,
		name        TEXT NOT NULL,
		cron        TEXT NOT NULL,
		agent_id    TEXT NOT NULL,
		payload     TEXT,
		enabled     INTEGER DEFAULT 1,
		last_run    DATETIME,
		next_run    DATETIME,
		run_count   INTEGER DEFAULT 0,
		catch_up    INTEGER DEFAULT 0,
		created_at  DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,

	// 006_webhooks
	`CREATE TABLE IF NOT EXISTS webhook_events (
		id          TEXT PRIMARY KEY,
		route       TEXT NOT NULL,
		method      TEXT NOT NULL,
		headers     TEXT,
		body        TEXT,
		received_at DATETIME NOT NULL DEFAULT (datetime('now')),
		dispatched  INTEGER DEFAULT 0
	)`,

	// 007_pageindex_nodes (flattened search index)
	`CREATE TABLE IF NOT EXISTS pageindex_nodes (
		id          TEXT PRIMARY KEY,
		namespace   TEXT NOT NULL,
		node_id     TEXT NOT NULL,
		parent_id   TEXT,
		path        TEXT,
		title       TEXT NOT NULL,
		summary     TEXT,
		search_text TEXT,
		raw_json    TEXT,
		created_at  DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_pi_nodes_ns ON pageindex_nodes(namespace)`,

	// 008_communication_channels
	`CREATE TABLE IF NOT EXISTS communication_channels (
		id          TEXT PRIMARY KEY,
		type        TEXT NOT NULL,
		agent_id    TEXT NOT NULL,
		config_json TEXT NOT NULL,
		status      TEXT NOT NULL DEFAULT 'disconnected',
		created_at  DATETIME NOT NULL DEFAULT (datetime('now')),
		updated_at  DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,

	// 009_channel_messages
	`CREATE TABLE IF NOT EXISTS channel_messages (
		id           TEXT PRIMARY KEY,
		channel_id   TEXT NOT NULL,
		channel_type TEXT NOT NULL,
		sender_id    TEXT,
		sender_name  TEXT,
		direction    TEXT NOT NULL,
		content      TEXT NOT NULL,
		reply_to_id  TEXT,
		metadata     TEXT,
		created_at   DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_chanmsg_channel ON channel_messages(channel_id)`,
	`CREATE INDEX IF NOT EXISTS idx_chanmsg_time ON channel_messages(created_at)`,

	// 010_coding_sessions
	`CREATE TABLE IF NOT EXISTS coding_sessions (
		id          TEXT PRIMARY KEY,
		tool_type   TEXT NOT NULL,
		session_id  TEXT NOT NULL,
		description TEXT,
		status      TEXT NOT NULL DEFAULT 'active',
		agent_id    TEXT NOT NULL,
		output      TEXT,
		created_at  DATETIME NOT NULL DEFAULT (datetime('now')),
		updated_at  DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_coding_agent ON coding_sessions(agent_id)`,
	`CREATE INDEX IF NOT EXISTS idx_coding_session ON coding_sessions(session_id)`,

	// 011_chat_history
	`CREATE TABLE IF NOT EXISTS chat_history (
		id           TEXT PRIMARY KEY,
		agent_id     TEXT NOT NULL,
		role         TEXT NOT NULL,
		content      TEXT NOT NULL,
		tool_calls   TEXT,
		tool_call_id TEXT,
		tokens       INTEGER DEFAULT 0,
		metadata     TEXT,
		created_at   DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_chat_agent ON chat_history(agent_id)`,
	`CREATE INDEX IF NOT EXISTS idx_chat_time ON chat_history(created_at)`,

	// 012_push_tokens
	`CREATE TABLE IF NOT EXISTS push_tokens (
		token       TEXT PRIMARY KEY,
		platform    TEXT,
		created_at  DATETIME NOT NULL DEFAULT (datetime('now')),
		updated_at  DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,

	// 013_proposals (human-in-the-loop approvals)
	`CREATE TABLE IF NOT EXISTS proposals (
		id              TEXT PRIMARY KEY,
		agent_id        TEXT NOT NULL,
		kind            TEXT,
		title           TEXT NOT NULL,
		summary         TEXT,
		context         TEXT,
		proposed_action TEXT,
		status          TEXT NOT NULL DEFAULT 'pending',
		decision_note   TEXT,
		result          TEXT,
		created_at      DATETIME NOT NULL DEFAULT (datetime('now')),
		decided_at      DATETIME
	)`,
	`CREATE INDEX IF NOT EXISTS idx_proposals_status ON proposals(status)`,
	`CREATE INDEX IF NOT EXISTS idx_proposals_time ON proposals(created_at)`,

	// 014_device_actions (on-device actions the phone app performs, e.g. EventKit)
	`CREATE TABLE IF NOT EXISTS device_actions (
		id          TEXT PRIMARY KEY,
		agent_id    TEXT NOT NULL,
		kind        TEXT NOT NULL,
		payload     TEXT NOT NULL,
		status      TEXT NOT NULL DEFAULT 'pending',
		result      TEXT,
		created_at  DATETIME NOT NULL DEFAULT (datetime('now')),
		done_at     DATETIME
	)`,
	`CREATE INDEX IF NOT EXISTS idx_device_actions_status ON device_actions(status)`,

	// 015_chat_summaries (cold per-chat context from background scans)
	`CREATE TABLE IF NOT EXISTS chat_summaries (
		chat_jid          TEXT PRIMARY KEY,
		chat_name         TEXT,
		is_group          INTEGER DEFAULT 0,
		summary           TEXT,
		message_count     INTEGER DEFAULT 0,
		own_message_count INTEGER DEFAULT 0,
		last_message_at   DATETIME,
		summarized_at     DATETIME,
		status            TEXT DEFAULT 'pending'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_chat_summaries_status ON chat_summaries(status)`,

	// 016_app_messages (the phone-app conversation thread, separate from the
	// agent's full working chat_history which also holds WhatsApp/loop turns)
	`CREATE TABLE IF NOT EXISTS app_messages (
		id         TEXT PRIMARY KEY,
		agent_id   TEXT NOT NULL,
		role       TEXT NOT NULL,
		content    TEXT NOT NULL,
		created_at DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_app_messages_agent ON app_messages(agent_id, created_at)`,

	// 017_memory_links (LLM-generated relationships between memory entries)
	`CREATE TABLE IF NOT EXISTS memory_links (
		from_id    TEXT NOT NULL,
		to_id      TEXT NOT NULL,
		relation   TEXT,
		created_at DATETIME NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (from_id, to_id, relation)
	)`,

	// 018_contacts (phone contact directory synced from the app, to resolve
	// WhatsApp numbers -> saved names)
	`CREATE TABLE IF NOT EXISTS contacts (
		phone      TEXT PRIMARY KEY,
		name       TEXT NOT NULL,
		updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,

	// 019_notifications (agent updates/alerts surfaced in the app feed; every
	// app.push is persisted here so the feed survives a missed/undelivered push)
	`CREATE TABLE IF NOT EXISTS notifications (
		id         TEXT PRIMARY KEY,
		agent_id   TEXT NOT NULL,
		kind       TEXT,
		title      TEXT,
		body       TEXT NOT NULL,
		data       TEXT,
		created_at DATETIME NOT NULL DEFAULT (datetime('now')),
		read_at    DATETIME
	)`,
	`CREATE INDEX IF NOT EXISTS idx_notifications_time ON notifications(created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_notifications_unread ON notifications(read_at)`,

	// 020_memory_metadata — structured fields for agentic recall: importance
	// (1=low..4=critical), pinned, usage reinforcement, TTL, and category. These
	// ADD COLUMNs are idempotent (migrate() tolerates "duplicate column name").
	`ALTER TABLE memory_entries ADD COLUMN category TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE memory_entries ADD COLUMN importance INTEGER NOT NULL DEFAULT 2`,
	`ALTER TABLE memory_entries ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE memory_entries ADD COLUMN access_count INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE memory_entries ADD COLUMN last_accessed_at DATETIME`,
	`ALTER TABLE memory_entries ADD COLUMN expires_at DATETIME`,
	`CREATE INDEX IF NOT EXISTS idx_mem_ns_importance ON memory_entries(namespace, importance)`,
	`CREATE INDEX IF NOT EXISTS idx_mem_expires ON memory_entries(expires_at)`,

	// reviews: open "is this still relevant?" clarification questions the agent
	// asks about STALE memory/reminders/commitments. Latched by target so an
	// item is never re-asked once answered; resolvable from app OR WhatsApp.
	`CREATE TABLE IF NOT EXISTS reviews (
		id           TEXT PRIMARY KEY,
		namespace    TEXT NOT NULL,
		target_kind  TEXT NOT NULL,          -- memory | reminder | commitment | task
		target_id    TEXT NOT NULL DEFAULT '',
		dedup_key    TEXT NOT NULL,          -- stable key so the same item is asked once
		question     TEXT NOT NULL,
		options      TEXT NOT NULL DEFAULT '[]',
		context      TEXT NOT NULL DEFAULT '',
		status       TEXT NOT NULL DEFAULT 'open',  -- open | resolved | dismissed
		answer       TEXT NOT NULL DEFAULT '',
		resolution   TEXT NOT NULL DEFAULT '',       -- kept | updated | forgotten | done | dropped
		created_at   DATETIME NOT NULL DEFAULT (datetime('now')),
		resolved_at  DATETIME
	)`,
	`CREATE INDEX IF NOT EXISTS idx_reviews_status ON reviews(namespace, status)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_reviews_dedup ON reviews(namespace, dedup_key)`,

	// 019_kv_memory — short-term scratch memory for loops. Namespaced by
	// "group" (a loop picks its own, e.g. one per chat), key/value chosen by the
	// caller, with an optional TTL. Durable (survives restarts) but expiring, so
	// it complements long-term memory_entries rather than replacing it.
	`CREATE TABLE IF NOT EXISTS kv_memory (
		grp        TEXT NOT NULL,
		key        TEXT NOT NULL,
		value      TEXT NOT NULL,
		expires_at DATETIME,
		created_at DATETIME NOT NULL DEFAULT (datetime('now')),
		updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (grp, key)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_kv_memory_grp ON kv_memory(grp)`,
	`CREATE INDEX IF NOT EXISTS idx_kv_memory_expires ON kv_memory(expires_at)`,

	// 020_gitloom_outbox — dropped.
	//
	// Memories used to be written to SQLite and queued here for delivery,
	// because GitLoom was a hosted service on the far side of home internet and
	// a blink must not lose a memory. It runs locally as part of KARMAX now, so
	// the queue buys nothing and the local copy it protected has become the
	// real cost: two stores that can disagree, silently, because a memory
	// forgotten in one and recalled from the other looks exactly like a memory
	// that was never forgotten.
	//
	// GitLoom is the store. SQLite keeps everything it was always for —
	// contacts, events, queues, timers, loop state, crash persistence.
	`DROP TABLE IF EXISTS gitloom_outbox`,

	// Where each local entry ended up in GitLoom. Retained because
	// memory_entries still holds history from before the move, and because an
	// install with no GitLoom configured keeps using that table as its store.
	`ALTER TABLE memory_entries ADD COLUMN gitloom_path TEXT NOT NULL DEFAULT ''`,
	`CREATE INDEX IF NOT EXISTS idx_mem_gitloom_path ON memory_entries(gitloom_path)`,

	// 021_loop_runs — durable loop execution.
	//
	// A loop run used to be a goroutine and a log line. When it failed the work
	// was gone, and the only trace was a WARN: 141 failures over three days,
	// all the same dead gateway, none retried and none visible while the daemon
	// reported itself healthy. A run is now a row that outlives the process.
	`CREATE TABLE IF NOT EXISTS loop_runs (
		id           TEXT PRIMARY KEY,
		loop         TEXT NOT NULL,
		trigger_kind TEXT NOT NULL DEFAULT '',
		status       TEXT NOT NULL DEFAULT 'running',  -- running | ok | failed | dead
		attempt      INTEGER NOT NULL DEFAULT 1,
		started_at   DATETIME NOT NULL,
		finished_at  DATETIME,
		duration_ms  INTEGER,
		error        TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_loop_runs_loop ON loop_runs(loop, started_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_loop_runs_status ON loop_runs(status, started_at DESC)`,

	// One row per loop: the lease that makes runs single-flight, and the
	// counters that let `karmax status` say a loop has gone dark.
	`CREATE TABLE IF NOT EXISTS loop_state (
		loop            TEXT PRIMARY KEY,
		lease_until     DATETIME,
		lease_owner     TEXT NOT NULL DEFAULT '',
		last_success_at DATETIME,
		last_failure_at DATETIME,
		consec_fails    INTEGER NOT NULL DEFAULT 0,
		next_retry_at   DATETIME,
		retry_attempt   INTEGER NOT NULL DEFAULT 0,
		last_error      TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_loop_state_retry ON loop_state(next_retry_at)`,

	// 022_mesh — other KARMAX instances, and this instance's stance toward them.
	//
	// This table is the access-control list. A verified signature proves who
	// sent an envelope; `state` decides whether that sender may say anything.
	// An instance not listed here, or listed and not accepted, is ignored —
	// default deny, so the mesh is opt-in per peer rather than open by default.
	`CREATE TABLE IF NOT EXISTS mesh_peers (
		id           TEXT PRIMARY KEY,          -- Ed25519 signing key: the identity
		name         TEXT NOT NULL DEFAULT '',  -- self-declared; display only
		box_pub      TEXT NOT NULL DEFAULT '',
		endpoint     TEXT NOT NULL DEFAULT '',
		state        TEXT NOT NULL DEFAULT 'pending', -- pending|accepted|blocked|revoked
		fingerprint  TEXT NOT NULL DEFAULT '',
		scopes       TEXT NOT NULL DEFAULT '',  -- what an accepted peer may do
		direction    TEXT NOT NULL DEFAULT 'in',
		note         TEXT NOT NULL DEFAULT '',
		created_at   DATETIME NOT NULL DEFAULT (datetime('now')),
		decided_at   DATETIME,
		last_seen_at DATETIME
	)`,
	`CREATE INDEX IF NOT EXISTS idx_mesh_peers_state ON mesh_peers(state)`,

	`CREATE TABLE IF NOT EXISTS mesh_messages (
		id          TEXT PRIMARY KEY,
		peer_id     TEXT NOT NULL,
		peer_name   TEXT NOT NULL DEFAULT '',
		kind        TEXT NOT NULL,
		direction   TEXT NOT NULL DEFAULT 'in',
		body        TEXT NOT NULL DEFAULT '',
		via         TEXT NOT NULL DEFAULT '',  -- org key, when sent under a certificate
		received_at DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_mesh_messages_time ON mesh_messages(received_at DESC)`,

	// 018_mesh_delegation
	`ALTER TABLE mesh_messages ADD COLUMN origin TEXT NOT NULL DEFAULT ''`,

	// 019_event_log — the durable replacement for the in-memory bus.
	`CREATE TABLE IF NOT EXISTS event_log (
		seq        INTEGER PRIMARY KEY AUTOINCREMENT,
		event_id   TEXT NOT NULL UNIQUE,
		workspace  TEXT NOT NULL DEFAULT 'default',
		kind       TEXT NOT NULL,
		agent_id   TEXT,
		payload    TEXT NOT NULL DEFAULT '{}',
		meta       TEXT NOT NULL DEFAULT '{}',
		created_at DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_event_log_read ON event_log(workspace, seq)`,
	`CREATE INDEX IF NOT EXISTS idx_event_log_kind ON event_log(workspace, kind, seq)`,
	`CREATE INDEX IF NOT EXISTS idx_event_log_agent ON event_log(workspace, agent_id, seq)`,

	`CREATE TABLE IF NOT EXISTS event_offsets (
		subscriber TEXT NOT NULL,
		workspace  TEXT NOT NULL DEFAULT 'default',
		seq        INTEGER NOT NULL DEFAULT 0,
		updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (subscriber, workspace)
	)`,

	`CREATE TABLE IF NOT EXISTS event_dead_letters (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		subscriber TEXT NOT NULL,
		event_seq  INTEGER NOT NULL,
		event_id   TEXT NOT NULL,
		kind       TEXT NOT NULL,
		attempts   INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_dead_letters_time ON event_dead_letters(created_at DESC)`,

	// Carry the old events table across so the operator's history survives the
	// switch. Ordered by time so seq matches the order things actually happened.
	`INSERT OR IGNORE INTO event_log (event_id, workspace, kind, agent_id, payload, meta, created_at)
	 SELECT id, 'default', kind, agent_id,
	        COALESCE(NULLIF(payload, ''), '{}'), COALESCE(NULLIF(meta, ''), '{}'), created_at
	 FROM events ORDER BY created_at ASC`,

	// 020_timers — "wait three days, then continue" as durable state.
	`CREATE TABLE IF NOT EXISTS timers (
		id         TEXT PRIMARY KEY,
		workspace  TEXT NOT NULL DEFAULT 'default',
		fire_at    DATETIME NOT NULL,
		kind       TEXT NOT NULL,
		agent_id   TEXT,
		loop       TEXT,
		payload    TEXT NOT NULL DEFAULT '{}',
		created_at DATETIME NOT NULL DEFAULT (datetime('now')),
		fired_at   DATETIME
	)`,
	`CREATE INDEX IF NOT EXISTS idx_timers_due ON timers(workspace, fired_at, fire_at)`,
	`CREATE INDEX IF NOT EXISTS idx_timers_loop ON timers(loop, fired_at)`,

	// 021_loop_steps — what a retry may skip because it already happened.
	`CREATE TABLE IF NOT EXISTS loop_steps (
		execution_id TEXT NOT NULL,
		loop         TEXT NOT NULL,
		name         TEXT NOT NULL,
		result       TEXT NOT NULL DEFAULT '',
		completed_at DATETIME NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (execution_id, name)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_loop_steps_age ON loop_steps(completed_at)`,
	// The execution survives across attempts; the run id does not.
	`ALTER TABLE loop_state ADD COLUMN execution_id TEXT NOT NULL DEFAULT ''`,

	// 022_capabilities — what each subject may do, and what it has used.
	`CREATE TABLE IF NOT EXISTS capability_grants (
		subject    TEXT NOT NULL,
		capability TEXT NOT NULL,
		value      TEXT NOT NULL,
		granted_by TEXT NOT NULL DEFAULT 'operator',
		granted_at DATETIME NOT NULL DEFAULT (datetime('now')),
		expires_at DATETIME,
		PRIMARY KEY (subject, capability, value)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_grants_subject ON capability_grants(subject, capability)`,

	// Aggregated per day: a row per tool call would be a disk-fill bug.
	`CREATE TABLE IF NOT EXISTS capability_meter (
		subject    TEXT NOT NULL,
		capability TEXT NOT NULL,
		day        TEXT NOT NULL,
		allowed    INTEGER NOT NULL DEFAULT 0,
		refused    INTEGER NOT NULL DEFAULT 0,
		units      INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (subject, capability, day)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_meter_day ON capability_meter(day DESC)`,

	// 023_connectors — KARMAX owns integration credentials, not the connectors.
	`CREATE TABLE IF NOT EXISTS connector_credentials (
		connector     TEXT PRIMARY KEY,
		config        TEXT NOT NULL DEFAULT '{}',
		access_token  TEXT NOT NULL DEFAULT '',
		refresh_token TEXT NOT NULL DEFAULT '',
		expires_at    DATETIME,
		enabled       INTEGER NOT NULL DEFAULT 0,
		updated_at    DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,
	// 024_org — the org chart as data, so hiring is a config action.
	`CREATE TABLE IF NOT EXISTS org_members (
		org        TEXT NOT NULL,
		member     TEXT NOT NULL,
		name       TEXT NOT NULL DEFAULT '',
		department TEXT NOT NULL DEFAULT '',
		role       TEXT NOT NULL DEFAULT '',
		namespace  TEXT NOT NULL DEFAULT '',
		added_at   DATETIME NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (org, member)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_org_members_dept ON org_members(org, department)`,

	`CREATE TABLE IF NOT EXISTS connector_cursors (
		connector  TEXT NOT NULL,
		source     TEXT NOT NULL,
		cursor     TEXT NOT NULL DEFAULT '',
		updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (connector, source)
	)`,

	// Every public post, kept whether it succeeded or was refused. This is the
	// record of what KARMAX said in the operator's name without being asked,
	// which is worth having in full: the rate limit reads it, and so does
	// anybody wondering what went out yesterday.
	`CREATE TABLE IF NOT EXISTS social_posts (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		platform  TEXT NOT NULL,
		posted_at DATETIME NOT NULL DEFAULT (datetime('now')),
		status    TEXT NOT NULL,
		post_id   TEXT NOT NULL DEFAULT '',
		text      TEXT NOT NULL DEFAULT '',
		detail    TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_social_posts_when ON social_posts(platform, posted_at DESC)`,

	// What each model call cost. The gateway KARMAX ran on reported zero for
	// every usage field, so nothing here was measurable and a 28k-token routing
	// decision looked identical to a 3k one. Metered inference makes that a
	// billing problem rather than an aesthetic one.
	`CREATE TABLE IF NOT EXISTS model_usage (
		id             TEXT PRIMARY KEY,
		agent_id       TEXT NOT NULL,
		provider       TEXT NOT NULL DEFAULT '',
		model          TEXT NOT NULL DEFAULT '',
		kind           TEXT NOT NULL DEFAULT '',
		input_tokens   INTEGER NOT NULL DEFAULT 0,
		output_tokens  INTEGER NOT NULL DEFAULT 0,
		cache_read     INTEGER NOT NULL DEFAULT 0,
		cache_write    INTEGER NOT NULL DEFAULT 0,
		created_at     DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_model_usage_when ON model_usage(created_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_model_usage_agent ON model_usage(agent_id, created_at DESC)`,

	// A turn is a row, for the same reason a loop run is one.
	//
	// The bus advances its offset when an event is handed to the agent's
	// mailbox, not when the turn finishes — so a crash mid-turn lost the work
	// AND the event: the subscriber resumed past it and it was never redelivered.
	// Loops already survive this via loop_runs + a lease; turns had nothing.
	`CREATE TABLE IF NOT EXISTS agent_turns (
		id           TEXT PRIMARY KEY,
		agent_id     TEXT NOT NULL,
		event_id     TEXT NOT NULL,
		event_kind   TEXT NOT NULL DEFAULT '',
		event_json   TEXT NOT NULL DEFAULT '',
		status       TEXT NOT NULL DEFAULT 'running',
		attempt      INTEGER NOT NULL DEFAULT 1,
		started_at   DATETIME NOT NULL,
		finished_at  DATETIME,
		error        TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_turns_status ON agent_turns(status, started_at)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_turns_agent ON agent_turns(agent_id, started_at DESC)`,
	// One row per event: a redelivered event must resume its turn, not start a
	// second one alongside the first.
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_turns_event ON agent_turns(event_id)`,

	// Work handed to a copy of the agent. The parent link is the point: today
	// the only record of "who asked for this" is an in-memory map with a 6h TTL,
	// so a restart forgets which turn caused which background job.
	`CREATE TABLE IF NOT EXISTS subagent_runs (
		id          TEXT PRIMARY KEY,
		parent_id   TEXT NOT NULL,
		child_id    TEXT NOT NULL,
		task        TEXT NOT NULL,
		status      TEXT NOT NULL DEFAULT 'running',
		result      TEXT NOT NULL DEFAULT '',
		depth       INTEGER NOT NULL DEFAULT 1,
		started_at  DATETIME NOT NULL,
		finished_at DATETIME
	)`,
	`CREATE INDEX IF NOT EXISTS idx_subagent_parent ON subagent_runs(parent_id, started_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_subagent_status ON subagent_runs(status)`,
}

func (s *Store) migrate() error {
	for _, stmt := range migrations {
		if _, err := s.db.Exec(stmt); err != nil {
			// ALTER TABLE ADD COLUMN is not idempotent; tolerate re-runs.
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}
