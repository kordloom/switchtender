package cmd

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
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
}

// licenseStatusCmd prints what the install is running under.
var licenseStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the tier this install runs, and when a license lapses.",
	RunE: func(_ *cobra.Command, _ []string) error {
		path := license.PathFor(licenseDB)
		lic, err := license.Load(path)
		if err != nil {
			return fmt.Errorf("the license at %s does not verify, so this install runs "+
				"Community: %w", path, err)
		}
		if lic == nil {
			fmt.Println("Community. Everything here is free to run, forever.")
			fmt.Println("Team unlocks SSO, the full policy engine, distributed workers, and the")
			fmt.Println("period change register: https://switchtender.com/pricing")
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
)

// licenseMintCmd signs a license. Hidden: it is the issuer's tool, useless without the private key,
// which never ships in a release.
var licenseMintCmd = &cobra.Command{
	Use:    "mint",
	Hidden: true,
	Short:  "Sign a license file with the issuer key.",
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
		now := time.Now().UTC()
		c := license.Claims{
			V: 1, ID: "lic_" + hex.EncodeToString(priv.Public().(ed25519.PublicKey)[:4]) +
				now.Format("20060102150405"),
			Org: mintOrg, Tier: mintTier, Hosts: mintHosts, Kid: license.IssuerKid,
			Issued:  now.Format(time.RFC3339),
			Expires: now.AddDate(0, 0, mintDays).Format(time.RFC3339),
		}
		raw, err := license.Sign(c, priv)
		if err != nil {
			return err
		}
		if err := os.WriteFile(mintOut, raw, 0o600); err != nil {
			return err
		}
		// Verified with the compiled-in key before it leaves the building, so a mint with a key
		// that does not match the build fails here rather than at the customer.
		if _, err := license.Verify(raw, mintOut); err != nil {
			return fmt.Errorf("minted license does not verify against this build: %w", err)
		}
		fmt.Printf("minted %s for %s (%s hosts, %d days) -> %s\n",
			mintTier, mintOrg, mintHosts, mintDays, mintOut)
		return nil
	},
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
	licenseMintCmd.Flags().StringVar(&mintTier, "tier", license.TierTeam, "team or enterprise.")
	licenseMintCmd.Flags().StringVar(&mintHosts, "hosts", "250", "Host band: 250, 1000, unlimited.")
	licenseMintCmd.Flags().IntVar(&mintDays, "days", 365, "Term length in days.")
	licenseMintCmd.Flags().StringVar(&mintOut, "out", "license.json", "Where to write the license.")
	licenseCmd.AddCommand(licenseMintCmd)
	rootCmd.AddCommand(licenseCmd)
}
