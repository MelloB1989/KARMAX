package setupagent

import (
	"sort"
	"strings"
)

// The recipes, one per service that cannot be connected in a single command.
//
// A recipe is a prompt and a list of binaries. It is deliberately not code: the
// pages these walk through are somebody else's product and they get redesigned,
// and an agent reading the page in front of it survives that in a way a
// selector written today does not.

// Recipes returns everything that can be connected this way, by id.
func Recipes() map[string]Recipe {
	out := map[string]Recipe{}
	for _, r := range []Recipe{google(), instagram(), linkedin()} {
		out[r.ID] = r
	}
	return out
}

// IDs lists the connectable services, sorted.
func IDs() []string {
	var ids []string
	for id := range Recipes() {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Lookup finds a recipe.
func Lookup(id string) (Recipe, bool) {
	r, ok := Recipes()[strings.TrimSpace(strings.ToLower(id))]
	return r, ok
}

// shared is the part every recipe says, so the rules about handing back to the
// person are written once.
const shared = `
You are setting this up ON THE OPERATOR'S BEHALF, in a browser they are already
using and already signed into. Rules for the whole task:

- NARRATE. Before each step, say in one short sentence what you are about to do,
  in plain words, as you would to someone watching over your shoulder. They are
  watching: your sentences are the progress bar.
- The browser is shared with them. Do not sign out of anything, do not change
  their settings, do not close tabs you did not open.
- When you reach something only they can do — a consent screen's Allow button,
  a password, a 2FA code, choosing between their accounts — STOP and say
  exactly what you need them to click, starting the sentence with "Your turn:".
  Then wait for the page to change. Do not try to click Allow yourself, and
  never type a password.
- If a page asks for payment details or wants to enable billing, STOP and say so.
  Nothing here should cost anybody money.
- Never print a secret, a token, a client secret or the contents of a
  credentials file into your replies. Move files with shell commands; do not
  read them out.
- If you get stuck for more than a couple of attempts on the same step, say
  plainly what is blocking you and stop. A clear stop is more useful than a
  loop.
- When you are finished, check your own work — by reading the page, or by
  running whatever check the steps below give you — and end with one line
  saying what is now connected. Do not claim something is connected because
  you performed the steps; claim it because you looked.
`

func google() Recipe {
	return Recipe{
		ID:    "google",
		Name:  "Google",
		Needs: []string{"gog"},
		Lede:  "Setting up Google. This one takes a few minutes — there is a project and an OAuth client to create.",
		Prompt: func(env Env) string {
			gog := env.Bin["gog"]
			account := env.Account
			who := "the account they are signed into in the browser"
			if account != "" {
				who = account
			}
			return `Connect the operator's Google account to KARMAX, using the gog CLI at ` + gog + `.

` + shared + `
Google has no shared app to sign into: every person who uses gog needs an OAuth
client of their own, created in their own Google Cloud project. That is what
most of this task is. The account to connect is ` + who + `.

Do it in this order, checking as you go:

1. Check what is already there: run ` + "`" + gog + ` auth list --json` + "`" + ` and
   ` + "`" + gog + ` auth credentials list` + "`" + `. If the account is already
   authorized, say so and stop — there is nothing to do.

2. Find out which Google APIs are needed. Run
   ` + "`" + gog + ` auth services --plain` + "`" + `. Use the rows for gmail,
   calendar, drive, docs, sheets, contacts and tasks; the APIS column names
   exactly which APIs to turn on. (Chat is Workspace-only — include it only if
   the account is a Workspace one, not a personal gmail.com address.)

3. In the browser, go to https://console.cloud.google.com/. Check who is signed
   in first — if it is not the right account, or nobody is, stop and ask them to
   sign in.

4. Create a Google Cloud project, or reuse one they already have that looks like
   it was made for this. Name a new one "KARMAX". Say which you did.

5. Enable each API from step 2 on that project.

6. Set up the OAuth consent screen (it may be called "Branding" or "Google Auth
   Platform"). User type External, publishing status Testing is fine. Add ` + who + `
   as a test user — without that, authorizing will fail at the last step with a
   403, which is the single most common way this goes wrong.

7. Create credentials: an OAuth client ID, application type "Desktop app". Name
   it "KARMAX". Download the JSON. It will land in their Downloads folder.

8. Give it to gog:
   ` + "`" + gog + ` auth credentials set <path to the downloaded json>` + "`" + `
   Then delete the downloaded file — it is a credential sitting in Downloads.

9. Authorize the account, using the two-step remote flow so the consent screen
   opens in THIS browser rather than whatever their system default is:
   a. ` + "`" + gog + ` auth add ` + who + ` --services gmail,calendar,drive,docs,sheets,contacts,tasks --remote --step 1` + "`" + `
      This prints a URL.
   b. Open that URL in the browser.
   c. Say "Your turn: sign in if asked, then click Continue and Allow." Wait.
      Google will warn that the app is unverified — that is expected for a
      client they made themselves; tell them to choose Advanced, then continue.
   d. When the page finishes, read the FULL URL of the page it landed on. It
      will be a localhost or 127.0.0.1 address with a code in it.
   e. ` + "`" + gog + ` auth add ` + who + ` --services gmail,calendar,drive,docs,sheets,contacts,tasks --remote --step 2 --auth-url "<that full URL>"` + "`" + `

10. Verify: ` + "`" + gog + ` auth list --json` + "`" + ` should now name the
    account. Say which account is connected and which services.`
		},
	}
}

func instagram() Recipe {
	return Recipe{
		ID:   "instagram",
		Name: "Instagram",
		Lede: "Setting up Instagram. Most of this is signing in — I will tell you when it is your turn.",
		Prompt: func(env Env) string {
			return `Get the operator signed into Instagram in this browser, so KARMAX can act as
them there.

` + shared + `
1. Go to https://www.instagram.com/ and see whether they are already signed in.
   If they are, say whose account it is and stop.
2. If not, say "Your turn: sign in to Instagram in the window that just opened,
   including any code it sends you." Then wait for the page to become their
   feed.
3. Confirm by reading the page: say which account is signed in.

Do not type a username, a password or a verification code yourself, even if you
somehow have one. Signing in is theirs to do; your job is to get them to the
right page and to confirm afterwards that it worked.`
		},
	}
}

func linkedin() Recipe {
	return Recipe{
		ID:   "linkedin",
		Name: "LinkedIn",
		Lede: "Setting up LinkedIn. Most of this is signing in — I will tell you when it is your turn.",
		Prompt: func(env Env) string {
			return `Get the operator signed into LinkedIn in this browser, so KARMAX can act as
them there.

` + shared + `
1. Go to https://www.linkedin.com/feed/ and see whether they are already signed
   in. If they are, say whose account it is and stop.
2. If not, say "Your turn: sign in to LinkedIn in the window that just opened,
   including any code it sends you." Then wait for the feed to load.
3. Confirm by reading the page: say which account is signed in.

Do not type a username, a password or a verification code yourself. Signing in
is theirs to do.`
		},
	}
}
