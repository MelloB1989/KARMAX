package store

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
}

func (s *Store) migrate() error {
	for _, stmt := range migrations {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}
