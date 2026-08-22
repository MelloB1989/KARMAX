package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/mesh"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// The operator's side of the mesh.
//
// Connecting to another instance is a trust decision, and a trust decision made
// without seeing what you are trusting is not one. So `connect` prints the
// remote fingerprint and stops, and `accept` requires the fingerprint to be
// typed back — the same shape as SSH's host-key prompt, for the same reason.

func meshCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mesh",
		Short: "Connect this instance to other KARMAX instances",
		Long: "Each instance is identified by a key, not a name. Before trusting one,\n" +
			"compare its fingerprint with its operator over a channel the network\n" +
			"cannot forge — a phone call, in person. An attacker who can intercept\n" +
			"the connection can also send you their own key.",
	}
	cmd.AddCommand(meshIDCmd(), meshConnectCmd(), meshPeersCmd(),
		meshAcceptCmd(), meshBlockCmd(), meshSendCmd(), meshBroadcastCmd(), meshInboxCmd(), meshOrgCmd())
	return cmd
}

// openMesh builds a node against the local store, for CLI use.
func openMesh() (*mesh.Node, *store.Store, error) {
	cfg, err := config.Load(findConfig())
	if err != nil {
		return nil, nil, err
	}
	db, err := store.New(cfg.DatabaseDSN(), zap.NewNop())
	if err != nil {
		return nil, nil, err
	}
	name := envOr("KARMAX_MESH_NAME", "karmax")
	if name == "karmax" && len(cfg.Agents) > 0 && cfg.Agents[0].ID != "" {
		name = cfg.Agents[0].ID
	}
	id, err := mesh.LoadOrCreateIdentity(filepath.Join(cfg.Karmax.DataDir, "mesh"), name)
	if err != nil {
		return nil, nil, err
	}
	node := mesh.New(id, mesh.Config{
		Endpoint:   envOr("KARMAX_MESH_ENDPOINT", ""),
		TrustedOrg: envOr("KARMAX_MESH_ORG_KEY", ""),
		IsOrg:      strings.EqualFold(envOr("KARMAX_MESH_IS_ORG", ""), "true"),
		OrgName:    envOr("KARMAX_MESH_ORG_NAME", ""),
	}, db, zap.NewNop())
	return node, db, nil
}

func meshIDCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "id",
		Short: "Show this instance's mesh identity",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			n, _, err := openMesh()
			if err != nil {
				return err
			}
			p := n.Public()
			fmt.Printf("name        %s\n", p.Name)
			fmt.Printf("fingerprint %s\n", p.Fingerprint)
			fmt.Printf("endpoint    %s\n", orNone(p.Endpoint))
			fmt.Printf("id          %s\n\n", p.ID)
			fmt.Println("Read the fingerprint aloud when someone is connecting to you.")
			if p.Endpoint == "" {
				fmt.Println("\nNo endpoint set: others cannot reach this instance.")
				fmt.Println("Set KARMAX_MESH_ENDPOINT to a URL that resolves to this host's webhook port.")
			}
			return nil
		},
	}
}

func meshConnectCmd() *cobra.Command {
	var note string
	cmd := &cobra.Command{
		Use:   "connect <endpoint>",
		Short: "Ask another instance to connect",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			n, _, err := openMesh()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
			defer cancel()

			peer, err := n.Connect(ctx, args[0], note)
			if err != nil {
				return err
			}
			fmt.Printf("request sent to %s\n\n", orNone(peer.Name))
			fmt.Printf("  fingerprint  %s\n\n", peer.Fingerprint)
			// The instruction matters more than the output: a fingerprint
			// nobody compares is decoration.
			fmt.Println("Confirm that fingerprint with their operator directly — by phone or in")
			fmt.Println("person, not over the same network you just used. Anyone who can")
			fmt.Println("intercept this connection can also answer it with their own key.")
			fmt.Println("\nThey must accept before either of you can exchange messages.")
			return nil
		},
	}
	cmd.Flags().StringVar(&note, "note", "", "a line for the other operator explaining who you are")
	return cmd
}

func meshPeersCmd() *cobra.Command {
	var state string
	cmd := &cobra.Command{
		Use:   "peers",
		Short: "List known instances and this instance's stance toward them",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			_, db, err := openMesh()
			if err != nil {
				return err
			}
			peers, err := db.ListMeshPeers(state)
			if err != nil {
				return err
			}
			if len(peers) == 0 {
				fmt.Println("no instances known yet — `karmax mesh connect <endpoint>` to reach one")
				return nil
			}
			fmt.Printf("%-16s %-10s %-24s %-22s %s\n", "NAME", "STATE", "FINGERPRINT", "SCOPES", "LAST SEEN")
			fmt.Println(strings.Repeat("─", 92))
			for _, p := range peers {
				last := "never"
				if p.LastSeenAt != nil {
					last = humanTime(p.LastSeenAt.Format(time.RFC3339))
				}
				fmt.Printf("%-16s %-10s %-24s %-22s %s\n",
					truncQ(orNone(p.Name), 16), p.State, p.Fingerprint,
					truncQ(strings.Join(p.Scopes, ","), 22), last)
			}
			pending := 0
			for _, p := range peers {
				if p.State == store.PeerPending && p.Direction == "in" {
					pending++
				}
			}
			if pending > 0 {
				fmt.Printf("\n%d instance(s) waiting on your decision — `karmax mesh accept <fingerprint>`\n", pending)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "filter: pending, accepted, blocked, revoked")
	return cmd
}

func meshAcceptCmd() *cobra.Command {
	var scopes []string
	cmd := &cobra.Command{
		Use:   "accept <fingerprint>",
		Short: "Admit an instance that asked to connect",
		Long: "The fingerprint must be typed in full, and must match what the other\n" +
			"operator reads back to you. Accepting by name or by position in a\n" +
			"list is what lets an attacker who timed their request well be\n" +
			"admitted instead of your colleague.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			n, db, err := openMesh()
			if err != nil {
				return err
			}
			peer, err := findPeerByFingerprint(db, args[0])
			if err != nil {
				return err
			}
			if peer.State == store.PeerBlocked {
				return fmt.Errorf("that instance is blocked; unblock it deliberately before accepting")
			}

			fmt.Printf("\n  name         %s\n", orNone(peer.Name))
			fmt.Printf("  fingerprint  %s\n", peer.Fingerprint)
			fmt.Printf("  endpoint     %s\n", orNone(peer.Endpoint))
			if peer.Note != "" {
				fmt.Printf("  note         %s\n", peer.Note)
			}
			fmt.Printf("  granting     %s\n\n", strings.Join(scopes, ", "))

			ctx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
			defer cancel()
			if err := n.Accept(ctx, peer.ID, scopes); err != nil {
				return err
			}
			fmt.Printf("connected to %s\n", orNone(peer.Name))
			return nil
		},
	}
	// Message-only by default. Admitting a colleague is not the same as letting
	// their instance query yours, so `ask` has to be asked for.
	cmd.Flags().StringSliceVar(&scopes, "scopes", []string{mesh.ScopeMessage, mesh.ScopeBroadcast},
		"what to allow: message, broadcast, ask")
	return cmd
}

func meshBlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "block <fingerprint>",
		Short: "Refuse an instance permanently",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			n, db, err := openMesh()
			if err != nil {
				return err
			}
			peer, err := findPeerByFingerprint(db, args[0])
			if err != nil {
				return err
			}
			if err := n.Block(peer.ID); err != nil {
				return err
			}
			// Blocked outranks an org certificate: an org cannot re-admit
			// someone this operator refused.
			fmt.Printf("blocked %s — further requests from it are dropped without asking again\n",
				orNone(peer.Name))
			return nil
		},
	}
}

func meshSendCmd() *cobra.Command {
	var subject string
	cmd := &cobra.Command{
		Use:   "send <fingerprint> <message...>",
		Short: "Send a message to a connected instance",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			n, db, err := openMesh()
			if err != nil {
				return err
			}
			peer, err := findPeerByFingerprint(db, args[0])
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
			defer cancel()
			if err := n.Send(ctx, peer.ID, mesh.KindMessage, mesh.MessageBody{
				Subject: subject, Text: strings.Join(args[1:], " "),
			}); err != nil {
				return err
			}
			fmt.Printf("sent to %s\n", orNone(peer.Name))
			return nil
		},
	}
	cmd.Flags().StringVar(&subject, "subject", "", "short label the receiving instance can route on")
	return cmd
}

func meshBroadcastCmd() *cobra.Command {
	var subject string
	cmd := &cobra.Command{
		Use:   "broadcast <message...>",
		Short: "Send to every connected instance",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			n, _, err := openMesh()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(c.Context(), 60*time.Second)
			defer cancel()

			res, err := n.Broadcast(ctx, mesh.MessageBody{
				Subject: subject, Text: strings.Join(args, " "),
			})
			if err != nil {
				return err
			}
			fmt.Printf("delivered to %d instance(s)\n", res.Delivered)
			// Partial failure is reported, never swallowed: "sent to the team"
			// when four of nine were unreachable is worse than an error,
			// because you stop looking.
			for name, why := range res.Failed {
				fmt.Printf("  ✗ %-16s %s\n", name, truncQ(why, 60))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&subject, "subject", "", "short label")
	return cmd
}

func meshInboxCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "inbox",
		Short: "Show recent messages exchanged with other instances",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			_, db, err := openMesh()
			if err != nil {
				return err
			}
			msgs, err := db.RecentMeshMessages(limit)
			if err != nil {
				return err
			}
			if len(msgs) == 0 {
				fmt.Println("no mesh messages yet")
				return nil
			}
			for _, m := range msgs {
				arrow := "←"
				if m.Direction == "out" {
					arrow = "→"
				}
				via := ""
				if m.Via != "" {
					via = " [via org]"
				}
				fmt.Printf("%s %s %-14s %s%s\n",
					m.ReceivedAt.Format("01-02 15:04"), arrow,
					truncQ(orNone(m.PeerName), 14), truncQ(oneLineStr(m.Body), 70), via)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 30, "how many to show")
	return cmd
}

// findPeerByFingerprint resolves the identifier an operator actually has.
//
// Matched case-insensitively against the full fingerprint, and against the key
// itself for scripts. Deliberately NOT a prefix match: two instances sharing a
// prefix would make "accept" ambiguous at the exact moment precision matters.
func findPeerByFingerprint(db *store.Store, want string) (*store.MeshPeer, error) {
	want = strings.ToUpper(strings.TrimSpace(want))
	peers, err := db.ListMeshPeers("")
	if err != nil {
		return nil, err
	}
	for i := range peers {
		if strings.ToUpper(peers[i].Fingerprint) == want || peers[i].ID == strings.TrimSpace(want) {
			return &peers[i], nil
		}
	}
	return nil, fmt.Errorf("no instance with fingerprint %q — check `karmax mesh peers`", want)
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unnamed)"
	}
	return s
}

func envOr(key, def string) string {
	ensureEnv()
	if v := strings.TrimSpace(osGetenv(key)); v != "" {
		return v
	}
	return def
}

func osGetenv(k string) string { return os.Getenv(k) }

// The org's side.
//
// An org reaches a member with no pairwise connection, so there is nothing to
// "connect" to and no peer list to pick from — it addresses an endpoint and
// proves its authority with a certificate. That only works where the member's
// operator put this org's key in their own config, which is the check that
// makes this a second trust root rather than a back door.
func meshOrgCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "org",
		Short: "Act as an organisation instance",
		Long: "Requires KARMAX_MESH_IS_ORG=true. Members must set KARMAX_MESH_ORG_KEY\n" +
			"to this instance's id before it can reach them — an org cannot mint\n" +
			"authority over an instance that never opted in.",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "key",
		Short: "Print the org key members must trust",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			n, _, err := openMesh()
			if err != nil {
				return err
			}
			p := n.Public()
			fmt.Printf("org          %s\n", p.Name)
			fmt.Printf("fingerprint  %s\n\n", p.Fingerprint)
			fmt.Println("Each member sets this in their own environment:")
			fmt.Printf("\n  KARMAX_MESH_ORG_KEY=%s\n\n", p.ID)
			fmt.Println("Give them the fingerprint by a route they can verify — this key is")
			fmt.Println("what lets this instance address them without their approval.")
			return nil
		},
	})

	var (
		scopes []string
		ttl    time.Duration
		subj   string
	)
	send := &cobra.Command{
		Use:   "send <member-endpoint> <message...>",
		Short: "Message a member directly, with no connection",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			n, _, err := openMesh()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
			defer cancel()

			// Ask the member who it is. An org keeps no directory it would have
			// to keep in sync; the instance itself is the authority on its keys.
			who, err := n.Hello(ctx, args[0])
			if err != nil {
				return err
			}
			cert, err := n.IssueFor(who.ID, scopes, ttl)
			if err != nil {
				return err
			}
			err = n.SendAsOrg(ctx, store.MeshPeer{
				ID: who.ID, Name: who.Name, BoxPub: who.BoxPub, Endpoint: args[0],
			}, cert, mesh.KindMessage, mesh.MessageBody{
				Subject: subj, Text: strings.Join(args[1:], " "),
			})
			if err != nil {
				// The likeliest cause by far, and worth naming rather than
				// leaving the operator to guess at a 403.
				return fmt.Errorf("%w\n\nIf this is a refusal, check that %s has "+
					"KARMAX_MESH_ORG_KEY set to this org's id — a member that has not "+
					"opted in refuses org messages by design", err, who.Name)
			}
			fmt.Printf("delivered to %s (%s)\n", orNone(who.Name), who.Fingerprint)
			fmt.Printf("  under a certificate valid for %s, scopes: %s\n", ttl, strings.Join(scopes, ","))
			return nil
		},
	}
	send.Flags().StringSliceVar(&scopes, "scopes", []string{mesh.ScopeMessage}, "what the certificate grants")
	send.Flags().DurationVar(&ttl, "ttl", time.Hour, "how long the certificate stays valid")
	send.Flags().StringVar(&subj, "subject", "", "short label")
	cmd.AddCommand(send)

	return cmd
}
