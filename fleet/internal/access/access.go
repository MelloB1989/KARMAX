// Package access is who may use the fleet from the tailnet: devices the
// operator tags, each an admin (everything) or a guest (only the sandbox
// agents lent to it). A tag binds the device's Tailscale StableID; its name
// is shown and never trusted. Untagged devices are refused, and their
// knocks are kept so the operator can tag them by the name fleetd saw.
package access

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/identity"
	"github.com/MelloB1989/karmax/fleet/internal/tailnet"
)

// Store keeps tags and knocks; the ledger is the real one.
type Store interface {
	Device(stableID string) (fleetapi.Device, bool, error)
	Devices() ([]fleetapi.Device, error)
	Knock(w tailnet.Who, now time.Time) (first bool, err error) // first knock in 24 h
	Knocks(limit int) ([]fleetapi.Knock, error)
	Tag(d fleetapi.Device) error // replaces role and agents
	Untag(stableID string) error
	Seen(stableID string, now time.Time) error
}

var (
	// ErrNoDevice is a device reference that names no known device.
	ErrNoDevice = errors.New("no such device")
	// ErrBadChange is a change that cannot be made as asked.
	ErrBadChange = errors.New("bad access change")
)

// Resolve is who a tailnet caller is: role "" means refused. The tag is the
// StableID's, and holds only while the device's owner is the login it was
// tagged with: a device that changes hands starts again untagged.
func Resolve(s Store, w tailnet.Who) (fleetapi.Device, error) {
	if w.StableID == "" || w.Login == "" {
		return fleetapi.Device{}, nil
	}
	d, ok, err := s.Device(w.StableID)
	if err != nil || !ok {
		return fleetapi.Device{}, err
	}
	if !strings.EqualFold(d.Login, w.Login) {
		return fleetapi.Device{}, nil
	}
	switch d.Role {
	case fleetapi.RoleAdmin, fleetapi.RoleGuest:
	default:
		return fleetapi.Device{}, nil
	}
	d.Name = w.Node // what the device is called now, for show
	return d, nil
}

// Refusal is what an untagged device is told: who fleetd sees, and what the
// operator runs to let it in.
func Refusal(w tailnet.Who) string {
	return fmt.Sprintf("this device (%s, %s) may not use this fleet yet.\nAsk the fleet's operator to run: fleetctl access lend %s <agent>",
		w.Node, w.Login, Short(w.Node))
}

// Short is a tailnet name's first label: friend-laptop for
// friend-laptop.tail1234.ts.net.
func Short(name string) string {
	s, _, _ := strings.Cut(name, ".")
	return s
}

// List is the tagged devices and the latest knocks of devices not tagged.
func List(s Store) (fleetapi.AccessList, error) {
	devs, err := s.Devices()
	if err != nil {
		return fleetapi.AccessList{}, err
	}
	knocks, err := s.Knocks(50)
	if err != nil {
		return fleetapi.AccessList{}, err
	}
	l := fleetapi.AccessList{Devices: devs, Knocks: []fleetapi.Knock{}}
	for _, k := range knocks {
		if !slices.ContainsFunc(devs, func(d fleetapi.Device) bool { return d.StableID == k.StableID }) {
			l.Knocks = append(l.Knocks, k)
		}
	}
	if l.Devices == nil {
		l.Devices = []fleetapi.Device{}
	}
	return l, nil
}

// Change applies an AccessChange made by `by`; sandbox(agent) says whether an
// agent may be lent. A device is admin or guest, never both: admin on a guest
// drops its lends, and lend on an admin is refused.
func Change(s Store, c fleetapi.AccessChange, by string, sandbox func(string) bool, now time.Time) (fleetapi.AccessList, error) {
	id, name, login, err := Find(s, c.Device)
	if err != nil {
		return fleetapi.AccessList{}, err
	}
	cur, tagged, err := s.Device(id)
	if err != nil {
		return fleetapi.AccessList{}, err
	}
	d := fleetapi.Device{StableID: id, Name: name, Login: login, TaggedAt: now, TaggedBy: by}
	agents := make([]string, len(c.Agents))
	for i, a := range c.Agents {
		agents[i] = identity.Base(strings.TrimSpace(a))
	}
	switch c.Op {
	case fleetapi.AccessAdmin:
		d.Role = fleetapi.RoleAdmin
	case fleetapi.AccessLend:
		if len(agents) == 0 {
			return fleetapi.AccessList{}, fmt.Errorf("%w: lend needs at least one agent", ErrBadChange)
		}
		if tagged && cur.Role == fleetapi.RoleAdmin {
			return fleetapi.AccessList{}, fmt.Errorf("%w: %s is an admin device, which has every agent: revoke it first to make it a guest", ErrBadChange, name)
		}
		for _, a := range agents {
			if !sandbox(a) {
				return fleetapi.AccessList{}, fmt.Errorf("%w: %s is not a sandbox agent: only agents with sandbox: true in fleet.yaml can be lent", ErrBadChange, a)
			}
		}
		d.Role, d.Agents = fleetapi.RoleGuest, union(cur.Agents, agents)
	case fleetapi.AccessUnlend:
		if !tagged || cur.Role != fleetapi.RoleGuest {
			return fleetapi.AccessList{}, fmt.Errorf("%w: %s is not a guest device: there is nothing lent to it", ErrBadChange, name)
		}
		if len(agents) == 0 {
			return fleetapi.AccessList{}, fmt.Errorf("%w: unlend needs at least one agent", ErrBadChange)
		}
		d.Role = fleetapi.RoleGuest
		for _, a := range cur.Agents {
			if !slices.Contains(agents, a) {
				d.Agents = append(d.Agents, a)
			}
		}
	case fleetapi.AccessRevoke:
		if !tagged {
			return fleetapi.AccessList{}, fmt.Errorf("%w: %s is not tagged", ErrBadChange, name)
		}
		if err := s.Untag(id); err != nil {
			return fleetapi.AccessList{}, err
		}
		return List(s)
	default:
		return fleetapi.AccessList{}, fmt.Errorf("%w: op %q: want admin, lend, unlend or revoke", ErrBadChange, c.Op)
	}
	if tagged && c.Op != fleetapi.AccessAdmin {
		d.TaggedAt, d.TaggedBy = cur.TaggedAt, cur.TaggedBy // a lend changes the agents, not when it was tagged
	}
	if err := s.Tag(d); err != nil {
		return fleetapi.AccessList{}, err
	}
	return List(s)
}

func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, x := range b {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

// Find is a device by full name, unique short name, or StableID, among tagged
// devices and knocks.
func Find(s Store, ref string) (stableID, name, login string, err error) {
	ref = strings.TrimSuffix(strings.TrimSpace(ref), ".")
	if ref == "" {
		return "", "", "", fmt.Errorf("%w: name a device", ErrBadChange)
	}
	devs, err := s.Devices()
	if err != nil {
		return "", "", "", err
	}
	knocks, err := s.Knocks(0)
	if err != nil {
		return "", "", "", err
	}
	type known struct{ id, name, login string }
	var all []known
	for _, d := range devs {
		all = append(all, known{d.StableID, d.Name, d.Login})
	}
	for _, k := range knocks {
		if !slices.ContainsFunc(all, func(x known) bool { return x.id == k.StableID }) {
			all = append(all, known{k.StableID, k.Name, k.Login})
		}
	}
	for _, x := range all {
		if x.id == ref || strings.EqualFold(x.name, ref) {
			return x.id, x.name, x.login, nil
		}
	}
	var hits []known
	for _, x := range all {
		if strings.EqualFold(Short(x.name), ref) {
			hits = append(hits, x)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0].id, hits[0].name, hits[0].login, nil
	case 0:
		return "", "", "", fmt.Errorf("%w %q: it has to ask first (any fleetctl command from it), then it is listed in fleetctl access", ErrNoDevice, ref)
	}
	names := make([]string, len(hits))
	for i, h := range hits {
		names[i] = h.name + " (" + h.id + ")"
	}
	return "", "", "", fmt.Errorf("%w: %q is %d devices: %s; use the full name or the id", ErrBadChange, ref, len(hits), strings.Join(names, ", "))
}

// Knocked records that an untagged device asked, and tells the operator the
// first time in a day it does (tell is the daemon's notifier).
func Knocked(s Store, w tailnet.Who, now time.Time, tell func(text string)) error {
	first, err := s.Knock(w, now)
	if err != nil {
		return err
	}
	if first && tell != nil {
		tell(fmt.Sprintf("%s (%s) asked for fleet access: fleetctl access lend %s <agent>, or admin", Short(w.Node), w.Login, Short(w.Node)))
	}
	return nil
}
