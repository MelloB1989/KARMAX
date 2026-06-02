package memory

import (
	"time"
)

type MemoryEntry struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	Namespace string    `json:"namespace"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Tags      []string  `json:"tags"`
	CreatedAt time.Time `json:"created_at"`
}

type SearchResult struct {
	Entry   MemoryEntry `json:"entry"`
	Score   float64     `json:"score"`
	Excerpt string      `json:"excerpt"`
}

type TreeNode struct {
	NodeID   string            `json:"node_id"`
	Title    string            `json:"title"`
	Summary  string            `json:"summary,omitempty"`
	Content  string            `json:"content,omitempty"`
	Metadata map[string]any    `json:"metadata,omitempty"`
	Children []*TreeNode       `json:"children,omitempty"`
}
