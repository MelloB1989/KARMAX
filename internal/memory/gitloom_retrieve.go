package memory

import (
	"fmt"

	"github.com/GitLoomHQ/gitloom-go/gitloom"
)

// A recall that found memories but delivered none.
//
// On 15 September GitLoom's retrieve API renamed its results from `hits` to
// `memories`. The SDK KARMAX was pinned to read `hits`, so every response
// decoded as empty — no error anywhere — and for two weeks every recall in
// KARMAX returned nothing while the service answered correctly. The calls, the
// agent, the loops: all of it worked from an empty memory.
//
// The server reports how many memories any retrieval arm produced and how many
// its relevance floor dropped. Whatever is left had to arrive. If it did not,
// the response carried results this client could not read — which is exactly
// what a renamed field looks like from here — and that is an error, not an
// empty memory.
func unreadRecall(res *gitloom.RecallResult) error {
	if res == nil {
		return fmt.Errorf("gitloom: recall returned no result")
	}
	delivered := len(res.Memories) + len(res.Defined)
	survived := res.Candidates - res.FilteredOut
	if delivered == 0 && survived > 0 {
		return fmt.Errorf("gitloom: recall found %d memories but delivered none — the response shape is not what this client reads", survived)
	}
	return nil
}
