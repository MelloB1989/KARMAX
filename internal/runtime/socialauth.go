package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/social"
	"github.com/MelloB1989/karmax/internal/wasmloop"
)

// Publishing, moved out of the agent's reach.
//
// linkedin.post and x.post used to be ordinary tools in the registry, which
// meant the orchestrator could hold them and every routing decision carried
// their schemas. They are now reachable only from a loop that declares the
// capability, and only one post at a time.
//
// The guard, the rate limit and the credential stay here rather than moving
// into the loop with the posting. A community-signed WASM blob publishes to
// strangers with nobody having read the text first; the check that refuses a
// post naming a client belongs where the loop cannot edit it.

// socialEndpoints is what each platform needs to be posted to.
var socialEndpoints = map[string]struct {
	endpoint string
	maxRunes int
}{
	"linkedin": {"https://api.linkedin.com/rest/posts", 3000},
	"x":        {"https://api.x.com/2/tweets", 280},
}

// SocialAuthorize clears one exact post and returns what is needed to send it.
//
// The guard and the token are one call on purpose. Handed out separately, a
// loop could fetch the credential and post text the guard never saw — the
// enforcement would be advisory. Here the text that was checked is the text the
// token is issued for.
func (k *wasmKit) SocialAuthorize(ctx context.Context, platform, text string) (wasmloop.SocialGrant, error) {
	platform = strings.ToLower(strings.TrimSpace(platform))
	spec, ok := socialEndpoints[platform]
	if !ok {
		return wasmloop.SocialGrant{}, fmt.Errorf("no such platform %q", platform)
	}
	if k.rt.socialGuard == nil || k.rt.socialLimit == nil {
		return wasmloop.SocialGrant{}, fmt.Errorf("publishing is not configured on this instance")
	}

	guard := k.rt.socialGuard.Guard()
	guard.MaxRunes = spec.maxRunes

	var grant wasmloop.SocialGrant
	out, err := social.Publish(platform, guard, k.rt.socialLimit, text, func() (string, string, error) {
		token, author, cerr := k.rt.socialCredential(ctx, platform)
		if cerr != nil {
			return "", "", cerr
		}
		grant = wasmloop.SocialGrant{
			Endpoint: spec.endpoint,
			Token:    token,
			Author:   author,
			Headers:  map[string]string{"Content-Type": "application/json"},
		}
		// No id or url yet: the loop has not posted. Recorded as authorized
		// rather than posted, so the log never claims something went out that
		// only got as far as being allowed to.
		return "", "", nil
	})
	if err != nil {
		return wasmloop.SocialGrant{}, err
	}
	// A dry run is a refusal to issue a credential, and the operator has
	// already been sent the draft by the path above.
	if dry, _ := out["dry_run"].(bool); dry {
		return wasmloop.SocialGrant{Refused: "dry run is on — the draft was sent to you instead. `karmax social dry-run off` to publish for real"}, nil
	}
	if grant.Token == "" {
		return wasmloop.SocialGrant{}, fmt.Errorf("%s is not connected — run `karmax login %s`", platform, platform)
	}
	return grant, nil
}

// socialCredential resolves the managed token for a platform.
//
// Read through the integration registry rather than from loop config, so
// `karmax login linkedin` remains the way credentials arrive and a refreshed
// token reaches the loop without anyone re-pasting anything. A loop asking for
// a platform nobody has connected gets told which command connects it.
func (rt *KarmaxRuntime) socialCredential(ctx context.Context, platform string) (token, author string, err error) {
	if rt.integrations == nil {
		return "", "", fmt.Errorf("integrations are not available on this instance")
	}
	creds, _, err := rt.integrations.Credentials(platform)
	if err != nil {
		return "", "", err
	}
	token = strings.TrimSpace(creds.AccessToken)
	if token == "" {
		return "", "", fmt.Errorf("%s has no stored credential", platform)
	}
	// LinkedIn needs the author URN in the body; X does not.
	author = strings.TrimSpace(creds.Get("member_urn"))
	return token, author, nil
}
