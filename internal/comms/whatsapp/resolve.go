package whatsapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/MelloB1989/karmax/internal/hostpaths"
)

// Turning a name into a conversation before sending.
//
// wacli has always resolved names on the way in, but its answer for "several
// people are called Dev" arrived as an error string with the candidates
// flattened into it. The caller could not act on that: it could not pick, and
// could not tell it apart from a name that matched nothing. So a perfectly
// ordinary "message Dev" ended as a failed send.
//
// Resolution happens here, against wacli's own decision (resolve?best=1), so
// the rule for "is one of these the clear winner" lives in one place and this
// side never guesses.

const resolveTimeout = 10 * time.Second

// resolveTarget turns a reference into a JID, or explains why it cannot.
//
// An address is left alone: a JID or phone number is already the answer, and a
// needless round trip is a needless way to fail.
func (w *WhatsAppChannel) resolveTarget(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || isAddress(ref) {
		return ref, nil
	}

	endpoint := strings.TrimRight(hostpaths.WacliAPIURL(), "/") +
		"/resolve?best=1&kind=chat&allow_direct=1&ref=" + url.QueryEscape(ref)

	reqCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ref, nil // not worth failing a send over
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// The bridge may be reachable by CLI even when the API is not. Fall
		// through and let `wacli send` resolve, which is what happened before
		// this existed.
		w.log.Debug("could not reach wacli to resolve a name; sending unresolved")
		return ref, nil
	}
	defer resp.Body.Close()

	var body struct {
		Match      resolvedMatch   `json:"match"`
		Candidates []resolvedMatch `json:"candidates"`
		Error      string          `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)

	switch resp.StatusCode {
	case http.StatusOK:
		if body.Match.JID == "" {
			return ref, nil
		}
		return body.Match.JID, nil

	case http.StatusConflict:
		return "", &comms.AmbiguousTargetError{Target: ref, Candidates: toCandidates(body.Candidates)}

	case http.StatusNotFound:
		return "", &comms.UnresolvedTargetError{Target: ref, Reason: body.Error}
	}
	// Any other status is wacli having a bad day, not a verdict on the name.
	return ref, nil
}

type resolvedMatch struct {
	JID     string `json:"jid"`
	Name    string `json:"name"`
	Phone   string `json:"phone"`
	IsGroup bool   `json:"is_group"`
}

func toCandidates(in []resolvedMatch) []comms.TargetCandidate {
	out := make([]comms.TargetCandidate, 0, len(in))
	for _, m := range in {
		out = append(out, comms.TargetCandidate{
			JID: m.JID, Name: m.Name, Phone: m.Phone, IsGroup: m.IsGroup,
		})
	}
	return out
}

// isAddress reports whether a reference already identifies a conversation: a
// JID, or a bare phone number.
func isAddress(ref string) bool {
	if strings.Contains(ref, "@") {
		return true
	}
	digits := strings.TrimPrefix(ref, "+")
	if digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
