package fleetapi

import "time"

// WeekEven is a fresh week's room: the whole weekly limit over seven days.
const WeekEven = 100.0 / 7

// WeekRoom is how much of its weekly limit an account can spend a day until
// its 7-day window resets, in percentage points: what is left over the days
// left. Room that resets unspent is lost, so a window an hour from its reset
// has the most; one whose reset is not known counts as a week away.
func WeekRoom(seven float64, resets, now time.Time) float64 {
	left := 7 * 24 * time.Hour
	if !resets.IsZero() {
		left = max(resets.Sub(now), time.Hour)
	}
	return max(0, 100-seven) / left.Hours() * 24
}
