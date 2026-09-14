package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MelloB1989/karmax/internal/browser"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/spf13/cobra"
)

// The browser, from a terminal.
//
// Mostly so the daemon is not the only way to reach it: somebody debugging why
// a connector will not authorize needs to be able to look at the window and
// see what is actually on it.

func browserCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "browser",
		Short: "The browser you and the assistant share",
		Long: "One Chromium, with a profile KARMAX owns, that you sign into and the assistant then\n" +
			"drives. Sign into Google, Instagram, LinkedIn — whatever you want it to reach — in\n" +
			"this window, and it will find those sessions already there.",
	}
	cmd.AddCommand(browserStatusCmd(), browserStartCmd(), browserOpenCmd(), browserStopCmd(),
		browserTabsCmd(), browserRequestsCmd(), browserRequestCmd(), browserFetchCmd(), browserEvalCmd())
	return cmd
}

func session() *browser.Session {
	dir := ""
	if cfg, err := config.Load(findConfig()); err == nil {
		dir = cfg.Karmax.DataDir
	}
	return browser.Shared(dir)
}

func browserStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Whether it is open, and what is on it",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := context.Background()
			s := session()
			bin := hostpaths.Browser()
			if bin == "" {
				fmt.Println("No Chrome, Chromium or Edge on this machine.")
				return nil
			}
			fmt.Println("browser: ", bin)
			fmt.Println("profile: ", s.Profile())
			if !s.Running(ctx) {
				fmt.Println("status:   not running")
				return nil
			}
			fmt.Println("status:   running")
			tabs, err := s.Tabs(ctx)
			if err != nil || len(tabs) == 0 {
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "\nTITLE\tURL")
			for _, t := range tabs {
				fmt.Fprintf(w, "%s\t%s\n", t.Title, t.URL)
			}
			return w.Flush()
		},
	}
}

func browserStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Open it",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := session().Start(context.Background()); err != nil {
				return err
			}
			fmt.Println("Open. Sign into whatever you want the assistant to reach.")
			return nil
		},
	}
}

func browserOpenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open <url>",
		Short: "Put a page in front of you",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tab, err := session().Open(context.Background(), args[0])
			if err != nil {
				return err
			}
			fmt.Println("Opened", tab.URL)
			return nil
		},
	}
}

func browserStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Close it (your sign-ins are kept)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := session().Stop(context.Background()); err != nil {
				return err
			}
			fmt.Println("Closed. What you signed into is still there next time.")
			return nil
		},
	}
}

// Network inspection: tabs/requests/request/fetch/eval. Unlike
// status/start/open/stop above, these are thin clients over
// POST /api/tools/browser — the same tool the model's own loop calls — not
// direct callers into internal/browser. That keeps one code path deciding
// what the browser is allowed to do, whether the caller is the harness or a
// terminal. Flag names here are a fixed contract a skills package documents
// verbatim; do not rename them.

// callBrowserTool posts one browser-tool action and returns its output
// object, or an error built from whatever the tool reported.
func callBrowserTool(input map[string]any, timeout time.Duration) (map[string]any, error) {
	out, err := apiPOSTJSON("/api/tools/browser", input, timeout)
	if err != nil {
		return nil, err
	}
	if ok, _ := out["ok"].(bool); !ok {
		return nil, fmt.Errorf("browser: %s", asStr(out["error"]))
	}
	body, _ := out["output"].(map[string]any)
	if body == nil {
		return nil, fmt.Errorf("browser: unexpected response shape")
	}
	return body, nil
}

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func browserTabsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "tabs",
		Short: "Open tabs, with their ids",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := callBrowserTool(map[string]any{"action": "tabs"}, 20*time.Second)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(out["tabs"])
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tTITLE\tURL")
			for _, tab := range asList(out["tabs"]) {
				fmt.Fprintf(w, "%s\t%s\t%s\n", asStr(tab["id"]), oneLine(asStr(tab["title"]), 40), asStr(tab["url"]))
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func browserRequestsCmd() *cobra.Command {
	var tab, urlSub, method, typ string
	var status, limit int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "requests",
		Short: "Captured network requests (newest first)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			input := map[string]any{"action": "requests", "limit": limit}
			if tab != "" {
				input["tab"] = tab
			}
			if urlSub != "" {
				input["url_contains"] = urlSub
			}
			if method != "" {
				input["method"] = method
			}
			if typ != "" {
				input["type"] = typ
			}
			if status != 0 {
				input["status"] = status
			}
			out, err := callBrowserTool(input, 20*time.Second)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(out["requests"])
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tMETHOD\tSTATUS\tTYPE\tURL")
			for _, r := range asList(out["requests"]) {
				status := ""
				if s, ok := r["status"]; ok {
					status = asStr(s)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					asStr(r["id"]), asStr(r["method"]), status, asStr(r["resource_type"]), asStr(r["url"]))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&tab, "tab", "", "filter by tab id")
	cmd.Flags().StringVar(&urlSub, "url", "", "filter: URL contains this substring")
	cmd.Flags().StringVar(&method, "method", "", "filter by HTTP method")
	cmd.Flags().StringVar(&typ, "type", "", "filter by resource type: xhr, fetch, document, other")
	cmd.Flags().IntVar(&status, "status", 0, "filter by exact HTTP status code")
	cmd.Flags().IntVar(&limit, "limit", 50, "max results")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func browserRequestCmd() *cobra.Command {
	var asJSON, asCurl bool
	var outFile string
	cmd := &cobra.Command{
		Use:   "request <id>",
		Short: "One captured request/response in full",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := callBrowserTool(map[string]any{"action": "request", "id": args[0]}, 20*time.Second)
			if err != nil {
				return err
			}
			if outFile != "" {
				body := asStr(out["response_body"])
				if err := os.WriteFile(outFile, []byte(body), 0o644); err != nil {
					return err
				}
				fmt.Printf("Wrote %d bytes to %s\n", len(body), outFile)
				return nil
			}
			if asCurl {
				fmt.Println(buildCurl(out))
				return nil
			}
			if asJSON {
				return printJSON(out)
			}
			fmt.Printf("%s %s\n", asStr(out["method"]), asStr(out["url"]))
			if s, ok := out["status"]; ok {
				fmt.Printf("status: %v %s\n", s, asStr(out["mime_type"]))
			}
			if b, _ := out["body_omitted"].(bool); b {
				fmt.Printf("body: omitted (%s)\n", asStr(out["body_omitted_reason"]))
			} else if body := asStr(out["response_body"]); body != "" {
				fmt.Println("\n" + body)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&asCurl, "curl", false, "print a curl reproduction (Cookie withheld)")
	cmd.Flags().StringVar(&outFile, "out", "", "write the response body to this file")
	return cmd
}

// buildCurl reproduces rec's request as a curl command. The Cookie header is
// withheld on purpose — it is the one header a captured request carries that
// nobody should be handed back verbatim on a terminal or in a script.
func buildCurl(rec map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "curl -X %s %s", asStr(rec["method"]), shellQuote(asStr(rec["url"])))

	headers, _ := rec["request_headers"].(map[string]any)
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	withheld := false
	for _, k := range keys {
		if strings.EqualFold(k, "cookie") {
			withheld = true
			continue
		}
		fmt.Fprintf(&b, " \\\n  -H %s", shellQuote(k+": "+asStr(headers[k])))
	}
	if body := asStr(rec["request_body"]); body != "" {
		fmt.Fprintf(&b, " \\\n  --data %s", shellQuote(body))
	}
	if withheld {
		b.WriteString("\n# Cookie header withheld — add your own -H 'Cookie: ...' if you need it")
	}
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func browserFetchCmd() *cobra.Command {
	var url, tab, method, from, body, outFile string
	var headers []string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "fetch",
		Short: "Replay a request from inside a tab, via its own fetch()",
		RunE: func(cmd *cobra.Command, _ []string) error {
			input := map[string]any{"action": "fetch"}
			if url != "" {
				input["url"] = url
			}
			if tab != "" {
				input["tab"] = tab
			}
			if method != "" {
				input["method"] = method
			}
			if from != "" {
				input["from"] = from
			}
			if len(headers) > 0 {
				input["headers"] = headers
			}
			// Presence of the flag is what matters, not whether the value is
			// empty — an explicit "" body still overrides one copied via
			// --from. See the browser tool's own handling of this same rule.
			if cmd.Flags().Changed("body") {
				b, err := readBodyArg(body)
				if err != nil {
					return err
				}
				input["body"] = b
			}
			out, err := callBrowserTool(input, 2*time.Minute)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(out)
			}
			respBody := asStr(out["body"])
			if outFile != "" {
				if err := os.WriteFile(outFile, []byte(respBody), 0o644); err != nil {
					return err
				}
				fmt.Printf("status %v — wrote %d bytes to %s\n", out["status"], len(respBody), outFile)
				return nil
			}
			fmt.Printf("status: %v\n\n%s\n", out["status"], respBody)
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "URL to request")
	cmd.Flags().StringVar(&tab, "tab", "", "tab to run fetch() in (default: the --from request's tab, else the most recently active tab)")
	cmd.Flags().StringVar(&method, "method", "", "HTTP method (default: GET, or the --from request's method)")
	cmd.Flags().StringVar(&from, "from", "", "copy method/headers/body from this captured request id (Cookie/Host/Content-Length excluded)")
	cmd.Flags().StringArrayVar(&headers, "header", nil, "extra 'Key: Value' header (repeatable)")
	cmd.Flags().StringVar(&body, "body", "", "request body: literal text, @file, or @- for stdin")
	cmd.Flags().StringVar(&outFile, "out", "", "write the response body to this file")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// readBodyArg resolves --body's three forms: a literal string, @file, or @-
// for stdin.
func readBodyArg(v string) (string, error) {
	switch {
	case v == "@-":
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	case strings.HasPrefix(v, "@"):
		b, err := os.ReadFile(strings.TrimPrefix(v, "@"))
		return string(b), err
	default:
		return v, nil
	}
}

func browserEvalCmd() *cobra.Command {
	var tab string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "eval <js>",
		Short: "Evaluate JavaScript in a tab, awaiting any returned promise",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			input := map[string]any{"action": "eval", "js": args[0]}
			if tab != "" {
				input["tab"] = tab
			}
			out, err := callBrowserTool(input, 2*time.Minute)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(out["result"])
			}
			// A plain string result prints unquoted — nicer for scripting —
			// anything else prints as indented JSON.
			if s, ok := out["result"].(string); ok {
				fmt.Println(s)
				return nil
			}
			return printJSON(out["result"])
		},
	}
	cmd.Flags().StringVar(&tab, "tab", "", "tab to evaluate in (default: the most recently active tab)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
