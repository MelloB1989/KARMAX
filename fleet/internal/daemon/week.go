package daemon

import (
	"fmt"
	"math"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// WeekRoom is an account's weekly room (fleetapi.WeekRoom): what picks weigh first.
var WeekRoom = fleetapi.WeekRoom

// weekTier ranks weekly room coarsely, so that weeks about as roomy count as
// level and the repo and the 5-hour window decide: tiers a factor of √2 wide,
// one of them centred on a fresh week's pace (12 to 17 %/day), where most
// weeks are.
func weekTier(room float64) int {
	return int(math.Round(2 * math.Log2(max(room, 0.1)/fleetapi.WeekEven)))
}

// weekWhy is an account's week for people: "7d 45%, 14%/day till Tue 05:30".
func weekWhy(seven, room float64, resets, now time.Time) string {
	if resets.IsZero() {
		return fmt.Sprintf("7d %.0f%%, %.0f%%/day", seven, room)
	}
	f := "15:04"
	if resets.Sub(now) > 20*time.Hour {
		f = "Mon 15:04"
	}
	return fmt.Sprintf("7d %.0f%%, %.0f%%/day till %s", seven, room, resets.Local().Format(f))
}
