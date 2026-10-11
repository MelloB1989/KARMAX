package console

import (
	"sort"
	"time"
)

// StreamLen is how many events the snapshot's stream keeps.
const StreamLen = 200

type evKey struct {
	agent, session, kind string
	sec                  int64
}

func keyOf(e Event) evKey {
	return evKey{e.Agent, e.Session, e.Kind, e.At.Truncate(time.Second).Unix()}
}

// mergeEvents joins the live tail (no ids) with fleetd's records (ids). The
// same hook event seen by both is one entry, with fleetd's id. Only stream
// kinds, newest first, at most StreamLen.
func mergeEvents(live, recorded []Event) []Event {
	seen := map[evKey]int{}
	var out []Event
	add := func(e Event) {
		if !IsStreamKind(e.Kind) {
			return
		}
		k := keyOf(e)
		if i, ok := seen[k]; ok {
			if out[i].ID == 0 && e.ID != 0 {
				out[i].ID = e.ID
			}
			return
		}
		seen[k] = len(out)
		out = append(out, e)
	}
	for _, e := range recorded {
		add(e)
	}
	for _, e := range live {
		add(e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].ID > out[j].ID
	})
	if len(out) > StreamLen {
		out = out[:StreamLen]
	}
	if out == nil {
		out = []Event{}
	}
	return out
}
