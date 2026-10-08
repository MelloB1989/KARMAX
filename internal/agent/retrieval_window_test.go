package agent

import (
	"context"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/memory"
)

func TestMemSearchReadsTheRangeAndScopeFromArgsAndContext(t *testing.T) {
	tool := &memSearchTool{}

	opts := tool.opts(context.Background(), map[string]any{
		"since": "2026-03-02", "until": "2026-03-02", "chat": "1234@s.whatsapp.net", "person": "Ab",
	})
	if opts.Since.IsZero() || !opts.Until.After(opts.Since.Add(23*time.Hour)) {
		t.Errorf("range = %v..%v, want the whole of the day", opts.Since, opts.Until)
	}
	if len(opts.TagsAll) != 2 || opts.TagsAll[0] != "chat:1234" || opts.TagsAll[1] != "person:ab" {
		t.Errorf("scope tags = %v", opts.TagsAll)
	}

	// The window memory.retrieve was given reaches the search the retriever runs.
	w := memory.Window{Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	got := tool.opts(memory.WithWindow(context.Background(), w), map[string]any{"query": "x"})
	if !got.Since.Equal(w.Since) || len(got.TagsAll) != 0 {
		t.Errorf("opts = %+v, want the context's window and no scope", got)
	}
}
