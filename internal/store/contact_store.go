package store

// Contact is one entry of the phone's contact directory (synced from the app),
// used to resolve WhatsApp numbers to the operator's saved names.
type Contact struct {
	Phone string `json:"phone"` // normalized digits
	Name  string `json:"name"`
}

// UpsertContacts inserts/updates contacts and returns how many were written.
func (s *Store) UpsertContacts(cs []Contact) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare(`INSERT INTO contacts (phone, name) VALUES (?, ?)
		ON CONFLICT(phone) DO UPDATE SET name=excluded.name, updated_at=datetime('now')`)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	defer stmt.Close()
	n := 0
	for _, c := range cs {
		if c.Phone == "" || c.Name == "" {
			continue
		}
		if _, err := stmt.Exec(c.Phone, c.Name); err != nil {
			tx.Rollback()
			return 0, err
		}
		n++
	}
	return n, tx.Commit()
}

// LookupContactName resolves a phone number (digits) to a saved name. It tries
// an exact match, then a last-10-digits suffix match to tolerate country-code
// differences (e.g. WhatsApp "12025550123" vs a contact saved as "2025550123").
func (s *Store) LookupContactName(phone string) string {
	if phone == "" {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var name string
	_ = s.db.QueryRow(`SELECT name FROM contacts WHERE phone = ? LIMIT 1`, phone).Scan(&name)
	if name == "" && len(phone) >= 10 {
		suffix := phone[len(phone)-10:]
		_ = s.db.QueryRow(`SELECT name FROM contacts WHERE phone LIKE ? LIMIT 1`, "%"+suffix).Scan(&name)
	}
	return name
}

// CountContacts returns how many contacts are synced.
func (s *Store) CountContacts() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM contacts`).Scan(&n)
	return n
}
