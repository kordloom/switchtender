package cmd

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/license"
)

// licenseDB resolves where this install's license lives, the same --db rule every command uses.
var licenseDB string

// licenseCmd groups the license commands.
var licenseCmd = &cobra.Command{
	Use:   "license",
	Short: "Show or install this install's license. No license means Community, which is complete.",
	Args:  cobra.NoArgs,
	RunE:  runGroupHelp,
}

// licenseStatusCmd prints what the install is running under.
var licenseStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the tier this install runs, and when a license lapses.",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		path := license.PathFor(licenseDB)
		lic, err := license.Load(path)
		if err != nil {
			return fmt.Errorf("the license at %s does not verify, so this install runs "+
				"Community: %w", path, err)
		}
		if lic == nil {
			fmt.Println("Community. Everything here is free to run, forever.")
			fmt.Println("Pro unlocks directory sign-in and five approval policies. Team adds the")
			fmt.Println("full policy engine, distributed workers, and the period change register:")
			fmt.Println("https://switchtender.com/pricing")
			return nil
		}
		state := "valid"
		if lic.Expired(time.Now()) {
			state = "lapsed; every Community feature keeps working and nothing was deleted"
		}
		fmt.Printf("%s license for %s (%s)\n", lic.Claims.Tier, lic.Claims.Org, state)
		fmt.Printf("  id       %s\n  hosts    %s\n  expires  %s\n  file     %s\n",
			lic.Claims.ID, lic.Claims.Hosts, lic.Claims.Expires, lic.Path)
		return nil
	},
}

// licenseInstallCmd verifies a license file and puts it where the install reads it.
var licenseInstallCmd = &cobra.Command{
	Use:   "install <license-file>",
	Short: "Verify a license file and install it beside the database.",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		raw, err := os.ReadFile(args[0])
		if err != nil {
			return fmt.Errorf("read license: %w", err)
		}
		// Verified before it is installed, so a bad file is refused here with the real reason
		// rather than silently degrading the server to Community on its next start.
		lic, err := license.Verify(raw, args[0])
		if err != nil {
			return err
		}
		dest := license.PathFor(licenseDB)
		if err := os.WriteFile(dest, raw, 0o600); err != nil {
			return fmt.Errorf("install license: %w", err)
		}
		fmt.Printf("installed: %s license for %s, expires %s\n  at %s\n",
			lic.Claims.Tier, lic.Claims.Org, lic.Claims.Expires, dest)
		fmt.Println("restart serve to pick it up; a lapse later needs no restart to drop safely")
		return nil
	},
}

// licenseMint flags.
var (
	mintKey, mintOrg, mintTier, mintHosts, mintOut string
	mintDays                                       int
	mintYes                                        bool
)

// mintBands are the fleet bands the pricing page publishes. Minting anything else would sign a
// license naming a band the product does not sell.
var mintBands = []string{"250", "1000", "unlimited"}

// licenseMintCmd signs a license. Hidden: it is the issuer's tool, useless without the private key,
// which never ships in a release.
var licenseMintCmd = &cobra.Command{
	Use:    "mint",
	Hidden: true,
	Short:  "Sign a license file with the issuer key.",
	Args:   cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		keyHex, err := os.ReadFile(mintKey)
		if err != nil {
			return fmt.Errorf("read issuer key: %w", err)
		}
		seed, err := hex.DecodeString(string(trimSpaceBytes(keyHex)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return fmt.Errorf("issuer key must be a %d-byte hex seed", ed25519.SeedSize)
		}
		priv := ed25519.NewKeyFromSeed(seed)
		if err := validateMint(); err != nil {
			return err
		}
		now := time.Now().UTC()
		id, err := mintID(priv.Public().(ed25519.PublicKey))
		if err != nil {
			return err
		}
		c := license.Claims{
			V: 1, ID: id,
			Org: mintOrg, Tier: mintTier, Hosts: mintHosts, Kid: license.IssuerKid,
			Issued:  now.Format(time.RFC3339),
			Expires: now.AddDate(0, 0, mintDays).Format(time.RFC3339),
		}
		raw, err := license.Sign(c, priv)
		if err != nil {
			return err
		}
		// Verified in memory, before anything reaches disk. Writing first and verifying after left a
		// complete-looking license on disk when verification failed, which is exactly the file
		// somebody then emails to a customer.
		if _, err := license.Verify(raw, mintOut); err != nil {
			return fmt.Errorf("minted license does not verify against this build: %w", err)
		}
		if err := confirmMint(c); err != nil {
			return err
		}
		if err := os.WriteFile(mintOut, raw, 0o600); err != nil {
			return err
		}
		fmt.Printf("minted %s %s for %s (%s hosts, %d days, expires %s) -> %s\n",
			c.ID, mintTier, mintOrg, mintHosts, mintDays, c.Expires[:10], mintOut)
		return nil
	},
}

// validateMint refuses a license the product does not sell. A mint is irreversible: the only
// revocation is retiring a signing key, which invalidates every license that key ever signed, so a
// wrong license stands for its whole term.
func validateMint() error {
	if strings.TrimSpace(mintOrg) == "" {
		return fmt.Errorf("--org is required and names the organization on the license")
	}
	if mintTier != license.TierPro && mintTier != license.TierTeam &&
		mintTier != license.TierEnterprise {
		return fmt.Errorf("tier %q is not pro, team, or enterprise", mintTier)
	}
	var band bool
	for _, b := range mintBands {
		if mintHosts == b {
			band = true
			break
		}
	}
	if !band {
		return fmt.Errorf("hosts %q is not a published band: %s",
			mintHosts, strings.Join(mintBands, ", "))
	}
	if mintDays <= 0 {
		return fmt.Errorf("--days must be positive, got %d, which would sign a license that "+
			"expired before it was issued", mintDays)
	}
	return nil
}

// mintID returns the license identifier. It carries the issuer key prefix, the timestamp, and
// random bytes, because the previous form collided for any two licenses the same key signed in the
// same second, and this field is the handle support and revocation conversations use.
func mintID(pub ed25519.PublicKey) (string, error) {
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("license id: %w", err)
	}
	return "lic_" + hex.EncodeToString(pub[:4]) + time.Now().UTC().Format("20060102150405") +
		hex.EncodeToString(nonce[:]), nil
}

// confirmMint shows what is about to be signed and waits for a yes, unless --yes was passed. The
// tier and the band are the two fields worth a second look, since one is the difference between a
// $490 license and a $30,000 one.
func confirmMint(c license.Claims) error {
	if mintYes {
		return nil
	}
	fmt.Printf("About to sign:\n  id      %s\n  org     %s\n  tier    %s\n  hosts   %s\n"+
		"  issued  %s\n  expires %s\n  out     %s\n",
		c.ID, c.Org, c.Tier, c.Hosts, c.Issued[:10], c.Expires[:10], mintOut)
	fmt.Print("Sign it? [y/N] ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("mint canceled: %w", err)
	}
	if strings.ToLower(strings.TrimSpace(line)) != "y" {
		return fmt.Errorf("mint canceled")
	}
	return nil
}

// trimSpaceBytes trims ASCII whitespace from a small key file.
func trimSpaceBytes(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\n' || b[start] == '\r' || b[start] == '\t') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\r' || b[end-1] == '\t') {
		end--
	}
	return b[start:end]
}

// init registers the license commands.
func init() {
	for _, c := range []*cobra.Command{licenseStatusCmd, licenseInstallCmd} {
		c.Flags().StringVar(&licenseDB, "db", defaultDBPath,
			"Database whose install the license belongs to; the file lives beside it.")
		licenseCmd.AddCommand(c)
	}
	licenseMintCmd.Flags().StringVar(&mintKey, "key", "", "Issuer private key file, hex seed.")
	licenseMintCmd.Flags().StringVar(&mintOrg, "org", "", "Organization the license names.")
	licenseMintCmd.Flags().StringVar(&mintTier, "tier", "", "pro, team, or enterprise.")
	licenseMintCmd.Flags().StringVar(&mintHosts, "hosts", "", "Host band: 250, 1000, unlimited.")
	licenseMintCmd.Flags().IntVar(&mintDays, "days", 0, "Term length in days.")
	licenseMintCmd.Flags().BoolVar(&mintYes, "yes", false, "Skip the confirmation prompt.")
	licenseMintCmd.Flags().StringVar(&mintOut, "out", "license.json", "Where to write the license.")
	for _, f := range []string{"key", "org", "tier", "hosts", "days"} {
		if err := licenseMintCmd.MarkFlagRequired(f); err != nil {
			panic("license mint: " + err.Error())
		}
	}
	licenseCmd.AddCommand(licenseMintCmd)
	rootCmd.AddCommand(licenseCmd)
}
