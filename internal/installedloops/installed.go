// Package installedloops blank-imports every installed loopkit loop so that
// each loop's init() runs and registers it. KARMAX reads the registry via
// loopkit.Registered() at startup.
//
// The import lines between the BEGIN/END markers are managed by
// `karmax loops install/remove` (and the TUI). You can edit them by hand, but
// the tooling keeps go.mod and the rebuild in sync — prefer it.
//
// KARMAX ships with four DEFAULT loops from the public marketplace
// (github.com/MelloB1989/karmax-loops): tech-news, hot-sync, profile-refresh,
// and daily-briefing. Remove any of them like an installed loop:
// `karmax loops remove <name>`.
package installedloops

import (
	// karmax:loops:begin
	_ "github.com/MelloB1989/karmax-loops/loops/daily-briefing"
	_ "github.com/MelloB1989/karmax-loops/loops/hot-sync"
	_ "github.com/MelloB1989/karmax-loops/loops/profile-refresh"
	_ "github.com/MelloB1989/karmax-loops/loops/tech-news"
	_ "github.com/MelloB1989/karmax-loops/loops/chat-sweep"
	_ "github.com/MelloB1989/karmax-loops/loops/gchat-watch"
	_ "github.com/MelloB1989/karmax-loops/loops/cold-scan"
	_ "github.com/MelloB1989/karmax-loops/loops/wa-monitor"
	// karmax:loops:end
)
