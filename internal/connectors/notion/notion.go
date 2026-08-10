// Package notion connects KARMAX to a Notion workspace.
//
// Deliberately a small tool surface. Notion's API is large and most of it is
// page-construction machinery an agent has no use for; what an assistant
// actually needs is to find something, read it, and add to it. Four tools that
// do that well beat forty that mirror the REST endpoints, because every tool is
// context in the prompt and a choice the model has to get right.
package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"github.com/jomei/notionapi"
)

// Connector is one Notion workspace.
type Connector struct{}

func New() *Connector { return &Connector{} }

func (c *Connector) Manifest() connectorkit.Manifest {
	return connectorkit.Manifest{
		ID:   "notion",
		Name: "Notion",
		Description: "Search, read and append to Notion pages and databases — so notes the operator keeps " +
			"there are reachable without them pasting anything.",
		Capabilities: []string{"http:api.notion.com"},
		Config: []connectorkit.ConfigField{
			{Key: "token", Description: "An internal integration secret (ntn_… or secret_…)", Required: true, Secret: true},
			{Key: "default_database", Description: "Database id used when a query omits one"},
		},
	}
}

// Auth is an internal integration secret rather than OAuth.
//
// OAuth exists for apps installed into other people's workspaces. KARMAX is
// installed into its operator's own, where the integration secret is what
// Notion hands you and there is nobody to authorise on behalf of.
func (c *Connector) Auth() connectorkit.AuthMethod {
	return connectorkit.AuthMethod{Kind: connectorkit.AuthAPIKey, APIKeyField: "token"}
}

func (c *Connector) Health(ctx context.Context, cr connectorkit.Credentials) error {
	client, err := clientFor(cr)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// Searching for nothing is the cheapest call that proves the token works
	// AND that the integration has been shared with at least something — which
	// is the failure people actually hit: a valid token that can see no pages,
	// because Notion requires each page to be shared with the integration.
	res, err := client.Search.Do(cctx, &notionapi.SearchRequest{PageSize: 1})
	if err != nil {
		return err
	}
	if len(res.Results) == 0 {
		return fmt.Errorf("the token works but the integration cannot see anything — " +
			"open a page in Notion, ⋯ → Connections → add this integration")
	}
	return nil
}

func (c *Connector) Tools() []connectorkit.Tool {
	return []connectorkit.Tool{
		{
			Name: "notion.search",
			Description: "Find pages and databases in Notion by title. Use before reading, " +
				"since Notion ids are opaque and nobody remembers them.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"query":{"type":"string","description":"Words from the title. Empty lists everything shared with KARMAX."},
					"limit":{"type":"integer","description":"Maximum results (default 10, max 50)."}
				}
			}`),
			Call: search,
		},
		{
			Name:        "notion.read",
			Description: "Read a Notion page's text. Takes the id from notion.search.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{"page_id":{"type":"string","description":"The page id."}},
				"required":["page_id"]
			}`),
			Call: read,
		},
		{
			Name:        "notion.append",
			Description: "Add a paragraph to the end of a Notion page. Use to record something where the operator keeps it.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"page_id":{"type":"string","description":"The page id."},
					"text":{"type":"string","description":"What to add."}
				},
				"required":["page_id","text"]
			}`),
			Call: appendBlock,
		},
		{
			Name:        "notion.query_database",
			Description: "List entries in a Notion database, newest first. Use for task lists and trackers.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"database_id":{"type":"string","description":"The database id; defaults to the configured one."},
					"limit":{"type":"integer","description":"Maximum entries (default 20, max 100)."}
				}
			}`),
			Call: queryDatabase,
		},
	}
}

// Sources is empty: Notion has no outbound webhooks for an internal
// integration, and polling every page would be a lot of requests to notice very
// little. A recipe on a schedule is the honest way to watch a Notion database.
func (c *Connector) Sources() []connectorkit.EventSource { return nil }

func clientFor(cr connectorkit.Credentials) (*notionapi.Client, error) {
	token := strings.TrimSpace(cr.Get("token"))
	if token == "" {
		return nil, fmt.Errorf("notion: no integration token configured")
	}
	return notionapi.NewClient(notionapi.Token(token)), nil
}

func search(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	client, err := clientFor(cr)
	if err != nil {
		return nil, err
	}
	query, _ := in["query"].(string)
	limit := intOf(in["limit"], 10)
	if limit > 50 {
		limit = 50
	}

	res, err := client.Search.Do(ctx, &notionapi.SearchRequest{
		Query: query, PageSize: limit,
	})
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(res.Results))
	for _, obj := range res.Results {
		switch v := obj.(type) {
		case *notionapi.Page:
			items = append(items, map[string]any{
				"id": v.ID.String(), "kind": "page", "title": pageTitle(v),
				"url": v.URL, "edited": v.LastEditedTime.Format(time.RFC3339),
			})
		case *notionapi.Database:
			items = append(items, map[string]any{
				"id": v.ID.String(), "kind": "database", "title": richText(v.Title),
				"url": v.URL, "edited": v.LastEditedTime.Format(time.RFC3339),
			})
		}
	}
	return map[string]any{"count": len(items), "items": items}, nil
}

func read(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	client, err := clientFor(cr)
	if err != nil {
		return nil, err
	}
	id, _ := in["page_id"].(string)
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("notion: a page_id is required")
	}

	children, err := client.Block.GetChildren(ctx, notionapi.BlockID(id), &notionapi.Pagination{PageSize: 100})
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, block := range children.Results {
		if line := blockText(block); line != "" {
			b.WriteString(line + "\n")
		}
	}
	return map[string]any{"page_id": id, "text": strings.TrimSpace(b.String())}, nil
}

func appendBlock(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	client, err := clientFor(cr)
	if err != nil {
		return nil, err
	}
	id, _ := in["page_id"].(string)
	text, _ := in["text"].(string)
	if strings.TrimSpace(id) == "" || strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("notion: a page_id and text are required")
	}

	_, err = client.Block.AppendChildren(ctx, notionapi.BlockID(id), &notionapi.AppendBlockChildrenRequest{
		Children: []notionapi.Block{
			notionapi.ParagraphBlock{
				BasicBlock: notionapi.BasicBlock{Object: "block", Type: "paragraph"},
				Paragraph: notionapi.Paragraph{
					RichText: []notionapi.RichText{{Type: "text", Text: &notionapi.Text{Content: text}}},
				},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"page_id": id, "appended": true}, nil
}

func queryDatabase(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	client, err := clientFor(cr)
	if err != nil {
		return nil, err
	}
	id, _ := in["database_id"].(string)
	if strings.TrimSpace(id) == "" {
		id = cr.Get("default_database")
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("notion: a database_id is required (or set default_database)")
	}
	limit := intOf(in["limit"], 20)
	if limit > 100 {
		limit = 100
	}

	res, err := client.Database.Query(ctx, notionapi.DatabaseID(id), &notionapi.DatabaseQueryRequest{
		PageSize: limit,
	})
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(res.Results))
	for _, page := range res.Results {
		p := page
		items = append(items, map[string]any{
			"id": p.ID.String(), "title": pageTitle(&p), "url": p.URL,
			"edited": p.LastEditedTime.Format(time.RFC3339),
		})
	}
	return map[string]any{"database_id": id, "count": len(items), "items": items}, nil
}

// pageTitle finds a page's title, whichever property holds it.
//
// Notion does not name the title property consistently — it is "Name" in a
// database and "title" on a plain page, and a user can rename it — so the type
// is what identifies it.
func pageTitle(p *notionapi.Page) string {
	for _, prop := range p.Properties {
		if t, ok := prop.(*notionapi.TitleProperty); ok {
			if s := richText(t.Title); s != "" {
				return s
			}
		}
	}
	return "(untitled)"
}

func richText(rt []notionapi.RichText) string {
	var b strings.Builder
	for _, r := range rt {
		b.WriteString(r.PlainText)
	}
	return strings.TrimSpace(b.String())
}

// blockText renders the block types that carry prose.
//
// The rest — images, embeds, columns — are skipped rather than rendered as
// placeholders, because a page read that is half "[unsupported block]" is
// harder for a model to use than one that is just the words.
func blockText(block notionapi.Block) string {
	switch b := block.(type) {
	case *notionapi.ParagraphBlock:
		return richText(b.Paragraph.RichText)
	case *notionapi.Heading1Block:
		return "# " + richText(b.Heading1.RichText)
	case *notionapi.Heading2Block:
		return "## " + richText(b.Heading2.RichText)
	case *notionapi.Heading3Block:
		return "### " + richText(b.Heading3.RichText)
	case *notionapi.BulletedListItemBlock:
		return "- " + richText(b.BulletedListItem.RichText)
	case *notionapi.NumberedListItemBlock:
		return "1. " + richText(b.NumberedListItem.RichText)
	case *notionapi.ToDoBlock:
		mark := " "
		if b.ToDo.Checked {
			mark = "x"
		}
		return "[" + mark + "] " + richText(b.ToDo.RichText)
	case *notionapi.QuoteBlock:
		return "> " + richText(b.Quote.RichText)
	case *notionapi.CodeBlock:
		return "```\n" + richText(b.Code.RichText) + "\n```"
	case *notionapi.CalloutBlock:
		return richText(b.Callout.RichText)
	}
	return ""
}

func intOf(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return def
}
