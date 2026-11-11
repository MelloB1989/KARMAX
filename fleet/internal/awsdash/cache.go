package awsdash

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// cacheFile is what is kept of a read: what the tab shows, no keys.
type cacheFile struct {
	Regions []string               `json:"regions"`
	Account string                 `json:"account"`
	User    string                 `json:"user"`
	Items   []fleetapi.AWSResource `json:"items"`
	Errs    []string               `json:"errs"`
	ResAt   time.Time              `json:"res_at"`
	Cost    *fleetapi.AWSCost      `json:"cost"`
	CostErr []string               `json:"cost_err"`
	CostAt  time.Time              `json:"cost_at"`
	CostOK  bool                   `json:"cost_ok"`
}

// load takes the last read from disk, so the tab has something to show, and
// Cost Explorer is not charged again for a cost already read, at start.
func (d *Dash) load() {
	if d.o.CacheFile == "" {
		return
	}
	b, err := os.ReadFile(d.o.CacheFile)
	if err != nil {
		return
	}
	var c cacheFile
	if json.Unmarshal(b, &c) != nil || c.Account == "" {
		return
	}
	d.regions, d.account, d.user, d.items, d.errs, d.resAt = c.Regions, c.Account, c.User, c.Items, c.Errs, c.ResAt
	d.cost, d.costErr, d.costAt, d.costOK = c.Cost, c.CostErr, c.CostAt, c.CostOK
}

// save keeps the last read on disk, 0600 and renamed in, so a crash leaves
// the old one whole.
func (d *Dash) save() {
	if d.o.CacheFile == "" {
		return
	}
	d.mu.Lock()
	c := cacheFile{d.regions, d.account, d.user, d.items, d.errs, d.resAt, d.cost, d.costErr, d.costAt, d.costOK}
	b, err := json.Marshal(c)
	d.mu.Unlock()
	if err != nil || c.Account == "" {
		return
	}
	tmp := d.o.CacheFile + ".tmp"
	if os.MkdirAll(filepath.Dir(tmp), 0o700) != nil || os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, d.o.CacheFile)
}
