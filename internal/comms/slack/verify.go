package slack

import (
	"net/http"

	"github.com/slack-go/slack"
)

// verifyRequest checks X-Slack-Signature/X-Slack-Request-Timestamp against
// secret. It's a thin wrapper over slack-go's own SecretsVerifier rather than
// a hand-rolled HMAC — v0 signing, the hmac.Equal comparison and the 5-minute
// staleness check are already implemented and tested there, and re-deriving
// them here would just be a second copy to keep in sync with Slack's docs. An
// empty secret fails closed (ErrInvalidConfiguration), so an install that
// hasn't set one rejects every HTTP request rather than accepting all of them.
func verifyRequest(header http.Header, body []byte, secret string) error {
	v, err := slack.NewSecretsVerifier(header, secret)
	if err != nil {
		return err
	}
	if _, err := v.Write(body); err != nil {
		return err
	}
	return v.Ensure()
}
