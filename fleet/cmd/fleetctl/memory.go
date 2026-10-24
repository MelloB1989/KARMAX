package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/MelloB1989/karmax/fleet/internal/memory"
)

const memoryUse = `memory recall "<query>" [--limit N] [--json] | memory suggest "<fact>" [--why "<why>"]`

// memoryCmd is `memory recall` (GET /v1/memory/recall) and `memory suggest`
// (POST /v1/memory/suggest): read the operator's long-term memory, and
// propose a fact to the orchestrator, which alone saves.
func memoryCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: fleetctl " + memoryUse)
	}
	switch args[0] {
	case "recall":
		var limit int
		var asJSON bool
		_, pos, err := flags("memory recall", args[1:], func(fs *flag.FlagSet) {
			fs.IntVar(&limit, "limit", 0, "")
			fs.BoolVar(&asJSON, "json", false, "")
		})
		if err != nil {
			return err
		}
		q := strings.Join(pos, " ")
		if strings.TrimSpace(q) == "" {
			return errors.New("usage: fleetctl " + memoryUse)
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		res, err := recall(c, q, limit)
		if err != nil {
			return err
		}
		if asJSON {
			return printJSON(res)
		}
		printRecall(os.Stdout, res)
		return nil
	case "suggest":
		var why string
		_, pos, err := flags("memory suggest", args[1:], func(fs *flag.FlagSet) { fs.StringVar(&why, "why", "", "") })
		if err != nil {
			return err
		}
		text := strings.Join(pos, " ")
		if strings.TrimSpace(text) == "" {
			return errors.New("usage: fleetctl " + memoryUse)
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		msg, err := suggest(c, text, why)
		if err != nil {
			return err
		}
		fmt.Println(msg)
		return nil
	}
	return errors.New("usage: fleetctl " + memoryUse)
}

func recall(c *client, q string, limit int) (*memory.Result, error) {
	v := url.Values{"q": {q}}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	var res memory.Result
	if err := c.do("GET", "/v1/memory/recall?"+v.Encode(), nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// suggest returns what to tell the caller: who has the fact now.
func suggest(c *client, text, why string) (string, error) {
	var out struct {
		To     string `json:"to"`
		Queued bool   `json:"queued"`
		Why    string `json:"why"`
	}
	if err := c.do("POST", "/v1/memory/suggest", map[string]string{"text": text, "why": why}, &out); err != nil {
		return "", err
	}
	if out.Queued {
		return "suggestion " + out.Why, nil
	}
	return "suggested to " + out.To + ", which decides whether to save it", nil
}

// printRecall writes memories for a person or a model to read.
func printRecall(w io.Writer, res *memory.Result) {
	if len(res.Memories) == 0 {
		fmt.Fprintf(w, "nothing in memory for %q\n", res.Query)
		return
	}
	for i, m := range res.Memories {
		if i > 0 {
			fmt.Fprintln(w)
		}
		head := m.Path
		if m.Title != "" {
			head += " — " + m.Title
		}
		if m.Score > 0 {
			head += fmt.Sprintf("  (%.2f)", m.Score)
		}
		fmt.Fprintln(w, head)
		for _, line := range strings.Split(strings.TrimRight(m.Content, "\n"), "\n") {
			fmt.Fprintln(w, "  "+line)
		}
	}
}
