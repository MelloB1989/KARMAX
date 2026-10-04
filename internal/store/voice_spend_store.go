package store

import "database/sql"

// Spend against a voice API key, keyed by a fingerprint of the key (never the key).

// AddVoiceSpend adds usd to the key's running total and returns the new total.
func (s *Store) AddVoiceSpend(keyFP string, usd float64) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.exec(`
INSERT INTO voice_spend (key_fp, spent_usd, updated_at) VALUES (?, ?, datetime('now'))
ON CONFLICT(key_fp) DO UPDATE SET
  spent_usd = voice_spend.spent_usd + excluded.spent_usd, updated_at = excluded.updated_at`,
		keyFP, usd); err != nil {
		return 0, err
	}
	return s.voiceSpentLocked(keyFP)
}

// VoiceSpent is the key's running total, zero if it has never spent.
func (s *Store) VoiceSpent(keyFP string) (float64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.voiceSpentLocked(keyFP)
}

func (s *Store) voiceSpentLocked(keyFP string) (float64, error) {
	var v sql.NullFloat64
	err := s.queryRow(`SELECT spent_usd FROM voice_spend WHERE key_fp = ?`, keyFP).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return v.Float64, err
}

// ClaimVoiceBudgetAlert reports true for exactly one caller per crossing.
func (s *Store) ClaimVoiceBudgetAlert(keyFP string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.exec(`INSERT INTO voice_spend (key_fp) VALUES (?) ON CONFLICT(key_fp) DO NOTHING`, keyFP); err != nil {
		return false, err
	}
	res, err := s.exec(`UPDATE voice_spend SET alerted = 1 WHERE key_fp = ? AND alerted = 0`, keyFP)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ClearVoiceBudgetAlert re-arms the alert, e.g. after the cap is raised.
func (s *Store) ClearVoiceBudgetAlert(keyFP string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`UPDATE voice_spend SET alerted = 0 WHERE key_fp = ? AND alerted = 1`, keyFP)
	return err
}
