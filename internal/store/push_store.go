package store

import "time"

// PushToken is a registered mobile push token (e.g. an Expo push token) used to
// deliver notifications to the operator's phone.
type PushToken struct {
	Token     string
	Platform  string
	UpdatedAt time.Time
}

// RegisterPushToken upserts a push token, refreshing its platform and timestamp.
func (s *Store) RegisterPushToken(token, platform string) error {
	_, err := s.db.Exec(
		`INSERT INTO push_tokens (token, platform) VALUES (?, ?)
		 ON CONFLICT(token) DO UPDATE SET platform = excluded.platform, updated_at = datetime('now')`,
		token, platform,
	)
	return err
}

// ListPushTokens returns all registered push tokens, most recently updated first.
func (s *Store) ListPushTokens() ([]PushToken, error) {
	rows, err := s.db.Query(`SELECT token, platform, updated_at FROM push_tokens ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []PushToken
	for rows.Next() {
		var t PushToken
		if err := rows.Scan(&t.Token, &t.Platform, &t.UpdatedAt); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}
