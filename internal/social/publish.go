package social

import "fmt"

// Publish is the single path from a draft to a public post.
//
// Both connectors go through here so the order is the same on every platform,
// and so adding a third one cannot accidentally get a different one. The order
// is: privacy, then the rate limit, then the post — privacy first because it is
// the feedback the model most needs, and a draft that names somebody should be
// told so even on a day when nothing could be posted anyway.
//
// Every outcome is recorded, including refusals.
func Publish(platform string, guard Guard, lim *Limiter, text string, do func() (id, url string, err error)) (map[string]any, error) {
	verdict := guard.Check(text)

	// A dry run shows the operator BOTH outcomes — the drafts that would have
	// gone out and the ones the guard stopped. Seeing only the survivors would
	// hide the thing most worth watching, which is what it tried to say.
	if dry, why := lim.dryRun(); dry {
		if lim.Preview == nil {
			return nil, &Refusal{Reason: "dry run is on but there is nowhere to send the draft"}
		}
		if err := lim.Preview(platform, text, verdict); err != nil {
			return nil, fmt.Errorf("social: could not send the draft to you: %w", err)
		}
		status := "preview"
		if verdict != nil {
			status = "preview-refused"
		}
		lim.Record(platform, status, "", text, verdict)
		return map[string]any{
			"dry_run": true, "posted": false, "platform": platform,
			"would_publish": verdict == nil, "note": why,
		}, nil
	}

	if verdict != nil {
		lim.Record(platform, "refused", "", text, verdict)
		return nil, verdict
	}
	if err := lim.Allow(platform); err != nil {
		lim.Record(platform, Status(err), "", text, err)
		return nil, err
	}

	id, url, err := do()
	if err != nil {
		lim.Record(platform, "failed", "", text, err)
		return nil, err
	}
	lim.Record(platform, "posted", id, text, nil)
	return map[string]any{"posted": true, "id": id, "url": url}, nil
}
