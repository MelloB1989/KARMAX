package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/term"

	"github.com/MelloB1989/karmax/fleet/internal/console"
	"github.com/MelloB1989/karmax/fleet/internal/sshdial"
)

// consoleConn is a way to fleet-console. FLEET_CONSOLE_URL and
// FLEET_CONSOLE_TOKEN reach it directly (on its own host, or a tunnel you
// made); otherwise fleet.yaml's console: section says where it runs, and the
// listener — loopback only — is reached through `ssh <host> -W`, with the
// client token read over the same ssh.
type consoleConn struct {
	base  string // http://…
	token string
	hc    *http.Client
}

func newConsole(ctx context.Context) (*consoleConn, error) {
	if u := os.Getenv("FLEET_CONSOLE_URL"); u != "" {
		tok := os.Getenv("FLEET_CONSOLE_TOKEN")
		if tok == "" {
			return nil, errors.New("FLEET_CONSOLE_URL is set without FLEET_CONSOLE_TOKEN")
		}
		return &consoleConn{base: strings.TrimRight(u, "/"), token: tok, hc: &http.Client{}}, nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	k := cfg.Console
	if k == nil {
		return nil, errors.New("no console: section in fleet.yaml (or set FLEET_CONSOLE_URL and FLEET_CONSOLE_TOKEN)")
	}
	tok, err := sshdial.Cat(ctx, k.Host, strings.TrimRight(k.Dir, "/")+"/client.token")
	if err != nil {
		return nil, fmt.Errorf("console token: %w", err)
	}
	return &consoleConn{base: "http://" + k.Listen, token: strings.TrimSpace(string(tok)), hc: sshdial.Client(k.Host, k.Listen)}, nil
}

// consoleConfigured reports whether attach should go through a console: the
// env override, or a console: section on the agent's host.
func consoleFor(agentHost string) bool {
	if os.Getenv("FLEET_CONSOLE_URL") != "" {
		return true
	}
	cfg, err := loadConfig()
	return err == nil && cfg.Console != nil && cfg.Console.Host == agentHost
}

func (c *consoleConn) header() http.Header {
	return http.Header{"Authorization": {"Bearer " + c.token}}
}

func (c *consoleConn) get(ctx context.Context, path string, out any) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.header()
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("console unreachable: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		var e struct{ Error string }
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return nil, errors.New(e.Error)
		}
		return nil, fmt.Errorf("console: %s", resp.Status)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func (c *consoleConn) dialWS(ctx context.Context, path string) (*websocket.Conn, *http.Response, error) {
	u := "ws" + strings.TrimPrefix(c.base, "http") + path
	return websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c.hc, HTTPHeader: c.header()})
}

// consoleCmd is `fleetctl console <snapshot|watch|term|health>`.
func consoleCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: fleetctl console snapshot|watch|term|health")
	}
	sub, args := args[0], args[1:]
	var asJSON bool
	_, pos, err := flags("console "+sub, args, func(fs *flag.FlagSet) { fs.BoolVar(&asJSON, "json", false, "") })
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch sub {
	case "snapshot", "health":
		c, err := newConsole(ctx)
		if err != nil {
			return err
		}
		rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if sub == "health" {
			var h struct {
				Sources map[string]console.Source `json:"sources"`
				Version string                    `json:"version"`
			}
			b, err := c.get(rctx, "/healthz", &h)
			if err != nil {
				return err
			}
			if asJSON {
				return printRaw(b)
			}
			fmt.Println("fleet-console", h.Version)
			printSources(os.Stdout, h.Sources, time.Now())
			return nil
		}
		var s console.Snapshot
		b, err := c.get(rctx, "/v1/snapshot", &s)
		if err != nil {
			return err
		}
		if asJSON {
			return printRaw(b)
		}
		printSnapshot(os.Stdout, &s)
		return nil

	case "watch":
		c, err := newConsole(ctx)
		if err != nil {
			return err
		}
		return consoleWatch(ctx, c, os.Stdout)

	case "term":
		if err := need(pos, 1, "console term <agent>"); err != nil {
			return err
		}
		c, err := newConsole(ctx)
		if err != nil {
			return err
		}
		return consoleTerm(ctx, c, pos[0], os.Stdout, os.Stderr)
	}
	return fmt.Errorf("unknown console command %q", sub)
}

func printRaw(b []byte) error {
	var v any
	if json.Unmarshal(b, &v) != nil {
		_, err := os.Stdout.Write(b)
		return err
	}
	return printJSON(v)
}

// consoleWatch prints each message the console sends as one JSON line.
func consoleWatch(ctx context.Context, c *consoleConn, w io.Writer) error {
	conn, resp, err := c.dialWS(ctx, "/v1/watch")
	if err != nil {
		return dialError(resp, err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 20)
	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				return nil
			}
			return err
		}
		var one json.RawMessage
		if json.Unmarshal(b, &one) != nil {
			continue
		}
		compact, _ := json.Marshal(one) // one line, whatever the console's spacing
		fmt.Fprintf(w, "%s\n", compact)
	}
}

// consoleTerm writes the agent's terminal bytes, raw, to out; frames that
// are not the stream go to errw.
func consoleTerm(ctx context.Context, c *consoleConn, agent string, out, errw io.Writer) error {
	conn, resp, err := c.dialWS(ctx, "/v1/term/"+url.PathEscape(agent))
	if err != nil {
		return dialError(resp, err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)
	for {
		typ, b, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				return nil
			}
			return err
		}
		if typ == websocket.MessageBinary {
			out.Write(b)
		} else if f, ok := parseFrame(b); ok && f.Type == "error" {
			fmt.Fprintln(errw, "console:", f.Error)
		}
	}
}

func parseFrame(b []byte) (console.TermFrame, bool) {
	var f console.TermFrame
	return f, json.Unmarshal(b, &f) == nil
}

// dialError turns a refused upgrade into what the console said.
func dialError(resp *http.Response, err error) error {
	if resp == nil {
		return fmt.Errorf("console unreachable: %w", err)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct{ Error, Holder string }
	json.Unmarshal(b, &e)
	switch {
	case resp.StatusCode == http.StatusConflict && e.Holder != "":
		return fmt.Errorf("held by %s", e.Holder)
	case e.Error != "":
		return errors.New(e.Error)
	}
	return fmt.Errorf("console: %s", resp.Status)
}

// consoleAttach gives this terminal the agent's keyboard through the
// console, so its one-controller rule binds here too. Ctrl+] gives it back.
func consoleAttach(ctx context.Context, c *consoleConn, agent string) error {
	who := "fleetctl"
	if u, err := user.Current(); err == nil {
		h, _ := os.Hostname()
		who = u.Username + "@" + h
	}
	conn, resp, err := c.dialWS(ctx, "/v1/term/"+url.PathEscape(agent)+"?control=1&who="+url.QueryEscape(who))
	if err != nil {
		return dialError(resp, err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return errors.New("attach needs a terminal")
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, old)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { // keystrokes
		defer cancel()
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 && conn.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		typ, b, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil || websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				return nil
			}
			return err
		}
		if typ == websocket.MessageBinary {
			os.Stdout.Write(b)
		} else if f, ok := parseFrame(b); ok {
			switch f.Type {
			case "released":
				fmt.Fprint(os.Stdout, "\r\n[released: you no longer have the keyboard]\r\n")
				return nil
			case "error":
				return errors.New(f.Error)
			}
		}
	}
}

// ---- human output ----

func printSnapshot(w io.Writer, s *console.Snapshot) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tPHASE\tTOOL\tCTX%\t5H%\t7D%\tCPU%\tMEM")
	for _, a := range s.Agents {
		tool, cpu, mem := "-", "-", "-"
		if !a.Live {
			tool, cpu, mem = "remote", "-", "-"
		} else {
			if a.CurrentTool != nil {
				tool = a.CurrentTool.Name + " " + age(s.At, a.CurrentTool.Since)
			}
			if a.Container != nil {
				cpu = fmt.Sprintf("%.0f", a.Container.CPUPct)
				mem = mib(a.Container.Mem)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, dash(a.Phase), tool,
			pctOf(a.ContextPct, a.Live), pctOf(a.FiveHourPct, a.Live), pctOf(a.SevenDayPct, a.Live), cpu, mem)
	}
	tw.Flush()
	fmt.Fprintln(w)
	printSources(w, s.Sources, s.At)
}

func printSources(w io.Writer, src map[string]console.Source, now time.Time) {
	names := make([]string, 0, len(src))
	for n := range src {
		names = append(names, n)
	}
	sort.Strings(names)
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SOURCE\tSTATE\tAGE\tERROR")
	for _, n := range names {
		s := src[n]
		state := "ok"
		if !s.OK {
			state = "DOWN"
		}
		a := "-"
		if !s.At.IsZero() {
			a = age(now, s.At)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n, state, a, s.Error)
	}
	tw.Flush()
}

func age(now, then time.Time) string {
	d := now.Sub(then).Round(time.Second)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

func pctOf(v float64, live bool) string {
	if !live {
		return "-"
	}
	return fmt.Sprintf("%.0f", v)
}

func mib(b int64) string { return fmt.Sprintf("%dM", b>>20) }
