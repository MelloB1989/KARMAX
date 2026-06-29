// Package installedloops blank-imports every installed loopkit loop so that
// each loop's init() runs and registers it. KARMAX reads the registry via
// loopkit.Registered() at startup.
//
// The import lines between the BEGIN/END markers are managed by the
// `karmax loops` TUI (install/remove). You can edit them by hand, but the TUI
// keeps go.mod and the rebuild in sync — prefer it.
package installedloops

import (
	// karmax:loops:begin
	_ "github.com/MelloB1989/karmax/internal/loops/hndigest"
	// karmax:loops:end
)
