package console

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// readHost reads the host's vitals from a /proc tree; root is "/proc" in
// production.
func readHost(root string) (HostInfo, error) {
	var h HostInfo
	b, err := os.ReadFile(filepath.Join(root, "loadavg"))
	if err != nil {
		return h, err
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return h, fmt.Errorf("loadavg: %q", b)
	}
	for i := range 3 {
		if h.Load[i], err = strconv.ParseFloat(f[i], 64); err != nil {
			return h, fmt.Errorf("loadavg: %w", err)
		}
	}
	b, err = os.ReadFile(filepath.Join(root, "uptime"))
	if err != nil {
		return h, err
	}
	up, err := strconv.ParseFloat(strings.Fields(string(b) + " ")[0], 64)
	if err != nil {
		return h, fmt.Errorf("uptime: %w", err)
	}
	h.UptimeS = int64(up)

	mf, err := os.Open(filepath.Join(root, "meminfo"))
	if err != nil {
		return h, err
	}
	defer mf.Close()
	var avail int64 = -1
	sc := bufio.NewScanner(mf)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fs := strings.Fields(rest)
		if len(fs) == 0 {
			continue
		}
		kb, _ := strconv.ParseInt(fs[0], 10, 64)
		switch key {
		case "MemTotal":
			h.MemTotal = kb * 1024
		case "MemAvailable":
			avail = kb * 1024
		}
	}
	if h.MemTotal == 0 || avail < 0 {
		return h, fmt.Errorf("meminfo: no MemTotal/MemAvailable")
	}
	h.MemUsed = h.MemTotal - avail
	h.CPUs = runtime.NumCPU()
	return h, nil
}
