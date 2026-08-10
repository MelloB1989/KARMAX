package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/wasmloop"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// Publishing and installing signed loops.

func wloopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wloop",
		Short: "Signed WASM loops: publish, install, inspect",
		Long: "A loop is a signed artifact containing a WASM module and a manifest of what it\n" +
			"needs. Installing verifies both signatures and the module's digest before any\n" +
			"byte reaches the compiler, and grants exactly what the manifest declared.",
	}
	cmd.AddCommand(wloopKeygenCmd(), wloopSignCmd(), wloopInspectCmd(),
		wloopInstallCmd(), wloopListCmd(), wloopRemoveCmd(), wloopVerifyAllCmd(),
		wloopTrustCmd())
	return cmd
}

// publisherKey is the on-disk signing identity. 0600, like the mesh identity.
type publisherKey struct {
	Public  string `json:"public"`
	Private string `json:"private"`
}

func keyPath() string { return filepath.Join(wasmloop.Dir(), "publisher.json") }

func loadPublisher() (ed25519.PrivateKey, string, error) {
	data, err := os.ReadFile(keyPath())
	if err != nil {
		return nil, "", fmt.Errorf("no publisher key — run `karmax wloop keygen` first")
	}
	var k publisherKey
	if err := json.Unmarshal(data, &k); err != nil {
		return nil, "", err
	}
	priv, err := base64.RawURLEncoding.DecodeString(k.Private)
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		return nil, "", fmt.Errorf("the publisher key file is malformed")
	}
	return ed25519.PrivateKey(priv), k.Public, nil
}

func wloopKeygenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "keygen",
		Short: "Create this machine's publishing identity",
		RunE: func(_ *cobra.Command, _ []string) error {
			if _, err := os.Stat(keyPath()); err == nil {
				_, pub, err := loadPublisher()
				if err != nil {
					return err
				}
				fmt.Printf("A publisher key already exists.\n\n  %s\n\nDelete %s to replace it.\n",
					pub, keyPath())
				return nil
			}
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				return err
			}
			k := publisherKey{
				Public:  base64.RawURLEncoding.EncodeToString(pub),
				Private: base64.RawURLEncoding.EncodeToString(priv),
			}
			data, _ := json.MarshalIndent(k, "", "  ")
			if err := os.MkdirAll(wasmloop.Dir(), 0o755); err != nil {
				return err
			}
			// 0600 before anything is written: a signing key must never exist
			// world-readable, even briefly.
			if err := os.WriteFile(keyPath(), data, 0o600); err != nil {
				return err
			}
			fmt.Printf("Publisher key created.\n\n  %s\n\nShare the public half; whoever installs your loops verifies against it.\n", k.Public)
			return nil
		},
	}
}

func wloopSignCmd() *cobra.Command {
	var manifestPath, modulePath, out string
	var countersign bool
	cmd := &cobra.Command{
		Use:   "sign",
		Short: "Package a WASM module and a manifest into a signed artifact",
		Long: "The manifest is YAML:\n\n" +
			"  name: tech-news\n" +
			"  version: 1.0.0\n" +
			"  description: a morning digest\n" +
			"  schedule: \"0 8 * * *\"\n" +
			"  memory_mb: 32\n" +
			"  host: [log, http, notify]\n" +
			"  capabilities:\n" +
			"    - http:hacker-news.firebaseio.com\n",
		RunE: func(_ *cobra.Command, _ []string) error {
			priv, pub, err := loadPublisher()
			if err != nil {
				return err
			}
			raw, err := os.ReadFile(manifestPath)
			if err != nil {
				return err
			}
			var m wasmloop.Manifest
			if err := yaml.Unmarshal(raw, &m); err != nil {
				return fmt.Errorf("%s: %w", manifestPath, err)
			}
			module, err := os.ReadFile(modulePath)
			if err != nil {
				return err
			}
			m.Publisher = pub
			if m.BuiltAt == 0 {
				m.BuiltAt = time.Now().Unix()
			}

			// Packed once to compute the digest the signature covers, then
			// signed and packed again — the digest is inside the signature, so
			// the order cannot be otherwise.
			probe, err := wasmloop.Pack(m, module, nil)
			if err != nil {
				return err
			}
			a, err := wasmloop.Unpack(probe)
			if err != nil {
				return err
			}
			sigs := []wasmloop.Signature{wasmloop.Sign(&a.Manifest, wasmloop.RolePublisher, priv)}
			if countersign {
				// Self-publishing: the operator is the reviewer for their own
				// loops. Countersigning with the same key means an instance that
				// trusts this key as a registry accepts them, while a stranger's
				// publisher-only artifact is still refused — which is narrower
				// than turning community trust on for everything.
				sigs = append(sigs, wasmloop.Sign(&a.Manifest, wasmloop.RoleRegistry, priv))
			}
			data, err := wasmloop.Pack(a.Manifest, module, sigs)
			if err != nil {
				return err
			}
			if out == "" {
				out = m.Name + "-" + m.Version + ".kloop"
			}
			if err := os.WriteFile(out, data, 0o644); err != nil {
				return err
			}
			role := "publisher"
			if countersign {
				role = "publisher and registry"
			}
			fmt.Printf("Signed %s (%s) as %s, %d KB.\n\n", m.Name, m.Version, role, len(data)/1024)
			fmt.Println("It declares:")
			for _, line := range wasmloop.Describe(m.Host, m.Capabilities) {
				fmt.Println("  - " + line)
			}
			fmt.Printf("\nWrote %s. Whoever installs it will see exactly that list.\n", out)
			return nil
		},
	}
	cmd.Flags().StringVar(&manifestPath, "manifest", "loop.yaml", "the manifest to sign")
	cmd.Flags().StringVar(&modulePath, "module", "loop.wasm", "the WASM module")
	cmd.Flags().StringVarP(&out, "out", "o", "", "output file")
	cmd.Flags().BoolVar(&countersign, "countersign", false,
		"also countersign as a registry, for loops you publish and trust yourself")
	return cmd
}

// trustFromEnv reads the shared trust configuration, with --allow-community as
// a per-command override.
func trustFromEnv(allowCommunity bool) wasmloop.Trust {
	t := wasmloop.LoadTrust(wasmloop.Dir())
	if allowCommunity {
		t.AllowCommunity = true
	}
	return t
}

func showPreview(p *wasmloop.Preview) {
	m := p.Manifest
	fmt.Printf("%s %s — %s\n", m.Name, m.Version, m.Description)
	fmt.Printf("  publisher %s\n", m.Publisher)
	switch p.Verdict.Tier {
	case wasmloop.TierRegistry:
		fmt.Printf("  trust     registry (countersigned by %s)\n", short(p.Verdict.Registry))
	default:
		fmt.Printf("  trust     COMMUNITY — nobody has reviewed this but its author\n")
	}
	if m.Schedule != "" {
		fmt.Printf("  runs      %s\n", m.Schedule)
	}
	fmt.Println("\nIt will be allowed to:")
	for _, line := range p.Grants {
		fmt.Println("  - " + line)
	}
	if p.Diff != nil {
		fmt.Printf("\nReplacing version %s.\n", p.Upgrade)
		if p.Diff.Same {
			fmt.Println("  Its permissions are unchanged.")
		}
		for _, a := range p.Diff.Added {
			fmt.Printf("  + NEW: %s\n", a)
		}
		for _, r := range p.Diff.Removed {
			fmt.Printf("  - no longer needs: %s\n", r)
		}
	}
}

func wloopInspectCmd() *cobra.Command {
	var allowCommunity bool
	cmd := &cobra.Command{
		Use:   "inspect <file.kloop>",
		Short: "Verify an artifact and show what installing it would allow",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			in := &wasmloop.Installer{Dir: wasmloop.Dir(), Trust: trustFromEnv(allowCommunity)}
			p, err := in.Inspect(data)
			if err != nil {
				return err
			}
			showPreview(p)
			fmt.Println("\nNothing has been installed.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&allowCommunity, "allow-community", false, "accept publisher-only signatures")
	return cmd
}

func wloopInstallCmd() *cobra.Command {
	var allowCommunity, yes bool
	cmd := &cobra.Command{
		Use:   "install <file.kloop>",
		Short: "Verify, record and activate a loop",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			in := &wasmloop.Installer{
				Dir: wasmloop.Dir(), Broker: brokerStore{s},
				Trust: trustFromEnv(allowCommunity), Actor: os.Getenv("USER"),
			}
			p, err := in.Inspect(data)
			if err != nil {
				return err
			}
			showPreview(p)

			if !yes {
				fmt.Print("\nInstall it? [y/N] ")
				var answer string
				fmt.Scanln(&answer)
				if answer != "y" && answer != "Y" {
					fmt.Println("Nothing installed.")
					return nil
				}
			}
			if _, err := in.Install(data); err != nil {
				return err
			}
			fmt.Printf("\nInstalled. Restart KARMAX to run it.\n")
			return nil
		},
	}
	cmd.Flags().BoolVar(&allowCommunity, "allow-community", false, "accept publisher-only signatures")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

// brokerStore adapts the store to what the installer needs.
type brokerStore struct{ s *store.Store }

func (b brokerStore) SaveGrant(g store.Grant) error         { return b.s.SaveGrant(g) }
func (b brokerStore) RevokeSubject(s string) (int64, error) { return b.s.RevokeSubject(s) }

func wloopListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show installed loops and how they were trusted",
		RunE: func(_ *cobra.Command, _ []string) error {
			in := &wasmloop.Installer{Dir: wasmloop.Dir()}
			entries, err := in.Installed()
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				fmt.Printf("No signed loops installed. The lockfile would be %s.\n", wasmloop.LockPath(wasmloop.Dir()))
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tVERSION\tTRUST\tPUBLISHER\tINSTALLED\tBY")
			for _, e := range entries {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Name, e.Version, e.Tier,
					short(e.Publisher), e.InstalledAt.Format("2006-01-02"), e.InstalledBy)
			}
			return w.Flush()
		},
	}
}

func wloopRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Uninstall a loop and revoke its capabilities",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()
			in := &wasmloop.Installer{Dir: wasmloop.Dir(), Broker: brokerStore{s}}
			if err := in.Remove(args[0]); err != nil {
				return err
			}
			fmt.Printf("Removed %s and revoked its capabilities. Restart KARMAX to unload it.\n", args[0])
			return nil
		},
	}
}

func wloopVerifyAllCmd() *cobra.Command {
	var allowCommunity bool
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Re-verify every installed loop against the lockfile",
		Long: "Checks that what is on disk is still what was approved. Run it if you suspect\n" +
			"anything has been tampered with; KARMAX also does this on every load.",
		RunE: func(_ *cobra.Command, _ []string) error {
			in := &wasmloop.Installer{Dir: wasmloop.Dir(), Trust: trustFromEnv(allowCommunity)}
			entries, err := in.Installed()
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				fmt.Println("Nothing installed.")
				return nil
			}
			bad := 0
			for _, e := range entries {
				if _, err := in.Load(e.Name); err != nil {
					bad++
					fmt.Printf("FAIL %s\n  %v\n", e.Name, err)
					continue
				}
				fmt.Printf("ok   %s %s (%s)\n", e.Name, e.Version, e.Tier)
			}
			if bad > 0 {
				return fmt.Errorf("%d loop(s) failed verification", bad)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&allowCommunity, "allow-community", false, "accept publisher-only signatures")
	return cmd
}

func wloopTrustCmd() *cobra.Command {
	var addRegistry, revoke string
	var allowCommunity, showOnly bool
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "Which publishers this instance accepts loops from",
		Long: "Read by both the CLI and the daemon, so they cannot disagree about whether a\n" +
			"loop is trusted.\n\n" +
			"  karmax wloop trust --registry <key>   accept loops countersigned by this key\n" +
			"  karmax wloop trust --revoke <key>     refuse anything signed by it\n\n" +
			"For loops you publish yourself, sign with --countersign and trust your own key:\n" +
			"that keeps a stranger's publisher-only artifact refused.",
		RunE: func(_ *cobra.Command, _ []string) error {
			dir := wasmloop.Dir()
			registries, revoked, community := wasmloop.StoredTrust(dir)

			changed := false
			if addRegistry != "" {
				registries = append(registries, addRegistry)
				changed = true
			}
			if revoke != "" {
				revoked = append(revoked, revoke)
				changed = true
			}
			if allowCommunity {
				community = true
				changed = true
			}
			if changed && !showOnly {
				if err := wasmloop.SaveTrust(dir, registries, revoked, community); err != nil {
					return err
				}
			}

			t := wasmloop.LoadTrust(dir)
			fmt.Printf("Trusted registries (%d):\n", len(t.Registries))
			for _, r := range t.Registries {
				fmt.Println("  " + r)
			}
			if len(t.Revoked) > 0 {
				fmt.Printf("\nRevoked (%d):\n", len(t.Revoked))
				for _, r := range t.Revoked {
					fmt.Println("  " + r)
				}
			}
			fmt.Printf("\nUnreviewed (publisher-only) loops: %s\n", allowed(t.AllowCommunity))
			fmt.Printf("Stored in %s\n", wasmloop.TrustPath(dir))
			if changed {
				fmt.Println("Restart KARMAX for the daemon to pick this up.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&addRegistry, "registry", "", "trust loops countersigned by this key")
	cmd.Flags().StringVar(&revoke, "revoke", "", "refuse anything signed by this key")
	cmd.Flags().BoolVar(&allowCommunity, "allow-community", false, "accept publisher-only loops from anyone")
	cmd.Flags().BoolVar(&showOnly, "dry-run", false, "show what would change without saving")
	return cmd
}

func allowed(b bool) string {
	if b {
		return "accepted"
	}
	return "refused"
}
