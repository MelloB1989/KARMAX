package social

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
	if err := guard.Check(text); err != nil {
		lim.Record(platform, "refused", "", text, err)
		return nil, err
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
