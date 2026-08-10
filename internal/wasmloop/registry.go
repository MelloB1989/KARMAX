package wasmloop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/safety"
)

// The registry: where loops come from.
//
// One index file listing both tiers, because an operator asking "what can I
// install" does not care which technology answers. A recipe is a YAML file and
// a workflow is a signed module, and the difference should show up in what the
// install costs them to approve, not in which command they have to know.
//
// Artifacts are NOT in the registry's git history. A .kloop is ~3MB and a
// registry that carries every version of every loop forever becomes a clone
// nobody wants. The index points at release downloads and pins each by digest,
// which is the same guarantee with none of the weight.

// DefaultRegistry is where loops are fetched from.
const DefaultRegistry = "https://raw.githubusercontent.com/MelloB1989/karmax-loops/main"

// IndexFile is the index's path within a registry.
const IndexFile = "/index.json"

// Kind distinguishes the two tiers.
type Kind string

const (
	// KindRecipe is a declarative YAML automation. No signature, no sandbox,
	// no install: it is read as data by KARMAX's own code.
	KindRecipe Kind = "recipe"
	// KindWorkflow is a signed WASM module.
	KindWorkflow Kind = "workflow"
)

// Index is a registry's catalogue.
type Index struct {
	Version int             `json:"version"`
	Entries []RegistryEntry `json:"entries"`
	// RegistryKey is the countersigning key this registry uses. Published here
	// so `karmax loops trust-registry` can offer it — an operator still has to
	// accept it, since a key that vouches for itself vouches for nothing.
	RegistryKey string `json:"registry_key,omitempty"`
}

// RegistryEntry is one installable thing in a registry, distinct from the
// lockfile Entry that records what is actually installed here.
type RegistryEntry struct {
	Name        string   `json:"name"`
	Kind        Kind     `json:"kind"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Author      string   `json:"author,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	// Source is the path in the registry repo, for anyone who wants to read it
	// before installing — which for an unsigned tier is the only review there is.
	Source string `json:"source,omitempty"`
	// Artifact is where the installable bytes are. Relative paths resolve
	// against the registry base; a workflow usually points at a release.
	Artifact string `json:"artifact"`
	// SHA256 pins the artifact. For a workflow this is belt and braces over the
	// signature; for a recipe it is the only integrity check there is, which
	// makes it the more important of the two.
	SHA256 string `json:"sha256,omitempty"`
	// ShipWithKARMAX marks what a fresh install starts with.
	ShipWithKARMAX bool `json:"ship_with_karmax,omitempty"`
	// Requires names tools the entry expects to exist, so "this needs WhatsApp
	// set up" is answerable before installing rather than after it fails.
	Requires []string `json:"requires,omitempty"`
}

// Client fetches from a registry.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient builds a registry client for the configured registry.
func NewClient() *Client {
	base := strings.TrimSpace(os.Getenv("KARMAX_REGISTRY"))
	if base == "" {
		base = DefaultRegistry
	}
	return &Client{
		BaseURL: strings.TrimSuffix(base, "/"),
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Index fetches and parses the catalogue.
func (c *Client) Index(ctx context.Context) (*Index, error) {
	body, err := c.get(ctx, c.BaseURL+IndexFile)
	if err != nil {
		return nil, fmt.Errorf("wasmloop: could not read the registry index: %w", err)
	}
	var idx Index
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("wasmloop: the registry index is not valid JSON: %w", err)
	}
	return &idx, nil
}

// Find returns one entry by name.
func (idx *Index) Find(name string) (RegistryEntry, bool) {
	for _, e := range idx.Entries {
		if strings.EqualFold(e.Name, name) {
			return e, true
		}
	}
	return RegistryEntry{}, false
}

// ShipWith returns the entries a fresh install starts with.
func (idx *Index) ShipWith() []RegistryEntry {
	var out []RegistryEntry
	for _, e := range idx.Entries {
		if e.ShipWithKARMAX {
			out = append(out, e)
		}
	}
	return out
}

// Fetch downloads an entry's artifact and checks it against the index.
//
// The digest is checked HERE, before the bytes go anywhere near an installer.
// For a workflow the signature would catch a swap anyway; for a recipe there
// is no signature, so this is the only thing standing between a compromised
// CDN and an automation running on the operator's behalf.
func (c *Client) Fetch(ctx context.Context, e RegistryEntry) ([]byte, error) {
	url := e.Artifact
	if url == "" {
		return nil, fmt.Errorf("wasmloop: %s has no artifact in the index", e.Name)
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = c.BaseURL + "/" + strings.TrimPrefix(url, "/")
	}
	body, err := c.get(ctx, url)
	if err != nil {
		return nil, err
	}
	if want := strings.ToLower(strings.TrimSpace(e.SHA256)); want != "" {
		if _, err := hex.DecodeString(want); err != nil || len(want) != 64 {
			return nil, fmt.Errorf("wasmloop: %s has an unusable sha256 in the index", e.Name)
		}
		if got := digest(body); got != want {
			return nil, fmt.Errorf("wasmloop: %s does not match what the registry index says\n"+
				"  index says %s\n  download is %s\nrefusing it", e.Name, want, got)
		}
	}
	return body, nil
}

// digest is the hex sha256 an index entry pins its artifact by.
func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// get performs one bounded, guarded fetch.
func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	// The same guard a loop's own egress passes. A registry URL comes from
	// configuration and an index the operator did not write, so "it is ours"
	// is not a reason to skip the check.
	if err := safety.CheckURL(url); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("not found at %s", url)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s answered %d", url, resp.StatusCode)
	}
	// Bounded: the length comes from a server, so it does not get to decide
	// how much memory this process uses.
	return io.ReadAll(io.LimitReader(resp.Body, maxModuleBytes))
}
