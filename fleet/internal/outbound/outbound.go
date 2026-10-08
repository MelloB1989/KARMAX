// Package outbound is everything fleetd says to the world outside the fleet:
// a signed webhook to KARMAX, a phone notification through `karmax notify`,
// the Fleet dashboard's data files, and the orchestrator's own quota read
// back from KARMAX. It also reads the env file the fleet's secrets live in.
package outbound

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

// Webhook posts state changes to a KARMAX webhooks.routes entry, which turns
// them into bus events — and so into orchestrator turns. No daemon code.
type Webhook struct {
	URL    string
	Secret string // the route's secret; empty sends unsigned
	Client *http.Client
}

// Push sends one state change.
func (w Webhook) Push(ctx context.Context, event, agent, text string, st reconcile.State) error {
	if w.URL == "" {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"event": event, "agent": agent, "text": text, "task": st.Task, "state": st.Phase,
		"five_hour_pct": st.Quota.FiveHour, "seven_day_pct": st.Quota.SevenDay, "at": time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if w.Secret != "" {
		mac := hmac.New(sha256.New, []byte(w.Secret))
		mac.Write(body)
		req.Header.Set("X-Fleet-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	c := w.Client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook %s: %s", w.URL, resp.Status)
	}
	return nil
}

// Karmax is the KARMAX CLI and API, from fleetd's side.
type Karmax struct {
	Bin      string
	APIURL   string
	APIToken string
	Client   *http.Client
	run      func(ctx context.Context, argv []string) error
}

// Notify pushes an alert to your phone.
func (k Karmax) Notify(ctx context.Context, text string) error {
	argv := []string{k.Bin, "notify", "--kind", "alert", "Agent fleet", text}
	if k.run != nil {
		return k.run(ctx, argv)
	}
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("karmax notify: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Quota is the orchestrator's own windows, as KARMAX's breaker last saw them.
type Quota struct {
	FiveHour, SevenDay             float64
	FiveHourResets, SevenDayResets time.Time
}

// Quota reads harness.list's rate_limits — the orchestrator's subscription,
// reported on every one of its harness turns.
func (k Karmax) Quota(ctx context.Context) (*Quota, error) {
	base := strings.TrimRight(k.APIURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/tools/harness.list", strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if k.APIToken != "" {
		req.Header.Set("Authorization", "Bearer "+k.APIToken)
	}
	c := k.Client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("harness.list: %s", resp.Status)
	}
	var out struct {
		OK     bool `json:"ok"`
		Output struct {
			RateLimits map[string]struct {
				Used     string `json:"used"`
				ResetsAt string `json:"resets_at"`
			} `json:"rate_limits"`
		} `json:"output"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	q := &Quota{}
	for name, w := range out.Output.RateLimits {
		used, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(w.Used), "%"), 64)
		resets, _ := time.Parse(time.RFC3339, w.ResetsAt)
		switch n := strings.ToLower(name); {
		case strings.Contains(n, "five") || strings.Contains(n, "5h"):
			q.FiveHour, q.FiveHourResets = used, resets
		case strings.Contains(n, "seven") || strings.Contains(n, "7d") || strings.Contains(n, "week"):
			q.SevenDay, q.SevenDayResets = used, resets
		}
	}
	return q, nil
}

// Dashboard writes the Fleet dashboard's data files. The dashboard itself is
// made once with KARMAX's dashboard tool; this only rewrites its data.
type Dashboard struct {
	DataDir string // KARMAX's data dir (~/.karmax)
	ID      string
}

// Write replaces data/<name>.json for each entry, atomically. It does nothing
// until the dashboard exists.
func (d Dashboard) Write(files map[string]any) error {
	if d.ID == "" {
		return nil
	}
	dir := filepath.Join(d.DataDir, "dashboards", d.ID)
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		return err
	}
	for name, v := range files {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		tmp := filepath.Join(data, "."+name+".json.tmp")
		if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, filepath.Join(data, name+".json")); err != nil {
			return err
		}
	}
	return nil
}

// ReadEnv reads a dotenv file: KEY=VALUE lines, optional export, quotes and
// trailing comments. Lines it does not understand are skipped.
func ReadEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		l = strings.TrimPrefix(l, "export ")
		k, v, ok := strings.Cut(l, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			continue
		}
		v = strings.TrimSpace(v)
		switch {
		case len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && strings.IndexByte(v[1:], v[0]) >= 0:
			v = v[1 : 1+strings.IndexByte(v[1:], v[0])]
		default:
			if i := strings.Index(v, " #"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
		}
		out[k] = v
	}
	return out, sc.Err()
}

// ParseDockerStats reads `docker stats --no-stream --format '{{json .}}'`.
func ParseDockerStats(b []byte, at time.Time) []ledger.HostSample {
	var out []ledger.HostSample
	for _, l := range bytes.Split(b, []byte("\n")) {
		var r struct{ Name, CPUPerc, MemUsage, MemPerc string }
		if json.Unmarshal(l, &r) != nil || r.Name == "" {
			continue
		}
		used, _, _ := strings.Cut(r.MemUsage, "/")
		out = append(out, ledger.HostSample{Container: r.Name, At: at, CPU: pct(r.CPUPerc),
			MemBytes: bytesOf(strings.TrimSpace(used)), MemPct: pct(r.MemPerc)})
	}
	return out
}

func pct(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
	return f
}

func bytesOf(s string) int64 {
	units := []struct {
		suffix string
		mult   float64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}, {"kB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"B", 1}}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			f, _ := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
			return int64(f * u.mult)
		}
	}
	f, _ := strconv.ParseFloat(s, 64)
	return int64(f)
}
