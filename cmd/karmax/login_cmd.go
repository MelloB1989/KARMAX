package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/integration"
	"github.com/MelloB1989/karmax/internal/integrations"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"golang.org/x/term"
)

// Connecting things, in one command.
//
// Slack wants two tokens pasted, Google wants a browser round trip, wacli holds
// its own session and only needs pairing. An operator should not have to know
// which of those they are in for before they can connect something.

func loginCmd() *cobra.Command {
	var account string
	var forget bool

	cmd := &cobra.Command{
		Use:   "login [integration]",
		Short: "Connect an integration — API key, browser sign-in, or a CLI session",
		Long: "Run it with no argument to see what can be connected and what already is.\n\n" +
			"Credentials are stored by KARMAX, and take precedence over anything in\n" +
			"karmax.yaml — so logging in wins over a key somebody set months ago.\n" +
			"`--forget` drops the stored one and hands control back to the file.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, cleanup, err := openIntegrations()
			if err != nil {
				return err
			}
			defer cleanup()

			if len(args) == 0 {
				return showIntegrations(cmd.Context(), reg, false)
			}
			id := args[0]
			if account != "" {
				id += ":" + account
			}
			if forget {
				if err := reg.Forget(id); err != nil {
					return err
				}
				fmt.Printf("Forgot the stored credentials for %s. karmax.yaml and the environment still apply.\n", id)
				return nil
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()
			return reg.Login(ctx, id, &terminalPrompter{})
		},
	}
	cmd.Flags().StringVar(&account, "account", "",
		"connect an additional account for this provider (e.g. --account work)")
	cmd.Flags().BoolVar(&forget, "forget", false, "drop the stored credentials for this integration")
	return cmd
}

func integrationsCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:     "integrations",
		Aliases: []string{"integration"},
		Short:   "What KARMAX is connected to, and whether it is working",
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg, cleanup, err := openIntegrations()
			if err != nil {
				return err
			}
			defer cleanup()
			return showIntegrations(cmd.Context(), reg, check)
		},
	}
	cmd.Flags().BoolVar(&check, "check", true, "run each health check (--check=false for the last known state)")
	return cmd
}

// showIntegrations prints the table both commands share.
func showIntegrations(ctx context.Context, reg *integration.Registry, check bool) error {
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	var statuses []integration.Status
	if check {
		statuses = reg.CheckAll(cctx)
	} else {
		statuses = reg.Statuses()
	}
	if len(statuses) == 0 {
		fmt.Println("Nothing is registered. Integrations appear here once the daemon has built them.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "INTEGRATION\tKIND\tAUTH\tSTATE\tCREDENTIALS FROM")
	needsLogin := 0
	for _, s := range statuses {
		state := "working"
		switch {
		case !s.Configured && s.AuthKind == connectorkit.AuthNone:
			state = "no login needed"
		case !s.Configured:
			state, needsLogin = "NOT CONNECTED", needsLogin+1
		case !s.Healthy:
			state, needsLogin = "FAILING", needsLogin+1
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.ID, s.Kind, s.AuthKind, state, s.Source)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// The reason each broken one is broken, under the table rather than
	// squeezed into a column — an expired-token message is a sentence, and
	// truncating it is how it stops being actionable.
	for _, s := range statuses {
		if s.Error != "" {
			fmt.Printf("\n%s: %s\n", s.ID, s.Error)
		}
	}
	if needsLogin > 0 {
		fmt.Printf("\n%d need attention. Connect one with `karmax login <integration>`.\n", needsLogin)
	}
	// A channel type only becomes an integration once a channel of that type is
	// configured, so one that is supported but unconfigured is missing from the
	// table entirely — which reads as "KARMAX cannot do Discord" rather than
	// "nothing has told it to".
	if cfg, err := config.Load(findConfig()); err == nil {
		if missing := integrations.UnconfiguredChannelTypes(cfg); len(missing) > 0 {
			fmt.Printf("\nAlso supported, but no channel configured: %s\n"+
				"Add one under comms.channels in karmax.yaml, then `karmax login <channel-id>`.\n",
				strings.Join(missing, ", "))
		}
	}

	// Browser sign-ins need this registered with the provider BEFORE the login
	// is attempted — otherwise the round trip dies on a redirect_uri mismatch
	// with no indication of what the URL should have been.
	for _, s := range statuses {
		if s.AuthKind == connectorkit.AuthOAuth2 && !s.Configured {
			fmt.Printf("\nBrowser sign-ins redirect back to: %s\n"+
				"Register that exact URL in the provider's app settings first.\n",
				integration.CallbackURL())
			break
		}
	}
	return nil
}

// terminalPrompter is the interactive half of a login.
type terminalPrompter struct{}

func (terminalPrompter) Say(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
}

func (terminalPrompter) Open(url string) error { return integration.OpenBrowser(url) }

// Ask reads one field, without echoing anything marked secret.
func (terminalPrompter) Ask(f connectorkit.ConfigField) (string, error) {
	label := f.Key
	if f.Description != "" {
		label += " (" + f.Description + ")"
	}
	if !f.Required {
		label += " [optional]"
	}

	if f.Secret && term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Printf("  %s: ", label)
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}

	fmt.Printf("  %s: ", label)
	var answer string
	fmt.Scanln(&answer)
	return strings.TrimSpace(answer), nil
}

// openIntegrations builds the same registry the daemon runs with.
//
// Built from the shared catalogue rather than asked of the daemon, so `karmax
// login` works before the daemon has ever started — which is the moment
// somebody is most likely to be connecting something.
func openIntegrations() (*integration.Registry, func(), error) {
	cfg, err := config.Load(findConfig())
	if err != nil {
		return nil, nil, err
	}
	db, err := store.New(filepath.Join(cfg.Karmax.DataDir, "db", "karmax.db"), zap.NewNop())
	if err != nil {
		return nil, nil, err
	}
	// The OAuth callback address is config, but the login flow lives in a
	// package that has never taken a config — so it is handed over the
	// environment, which the operator can also set directly.
	if p := cfg.Karmax.OAuthCallbackPort; p > 0 && os.Getenv("KARMAX_OAUTH_CALLBACK_PORT") == "" {
		_ = os.Setenv("KARMAX_OAUTH_CALLBACK_PORT", strconv.Itoa(p))
	}
	if h := strings.TrimSpace(cfg.Karmax.OAuthCallbackHost); h != "" && os.Getenv("KARMAX_OAUTH_CALLBACK_HOST") == "" {
		_ = os.Setenv("KARMAX_OAUTH_CALLBACK_HOST", h)
	}
	return integrations.Build(cfg, db), func() { db.Close() }, nil
}
