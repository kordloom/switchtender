package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/jsonutil"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/witnessclient"
)

var (
	// witnessEnrollDB is the database whose install enrolls. The license file, the producer key, and
	// the enrollment file are all located from it.
	witnessEnrollDB = defaultDBPath
	// witnessEnrollURL is the base URL of the witness to enroll with.
	witnessEnrollURL string
	// witnessEnrollKey pins the witness key id the enrollment must name as current.
	witnessEnrollKey string
	// witnessEnrollRefresh fetches the witness's key list instead of enrolling.
	witnessEnrollRefresh bool
)

// witnessKeyIDForm matches a witness key id as the witness publishes it.
var witnessKeyIDForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// witnessEnrollTimeout bounds the whole enrollment exchange with the witness.
const witnessEnrollTimeout = time.Minute

// witnessEnrollCmd enrolls this install with a push witness.
var witnessEnrollCmd = &cobra.Command{
	Use:   "enroll",
	Short: "Enroll this install with a push witness that countersigns its chain.",
	Long: `Enroll this install with a push witness that countersigns its chain.

An enrolled install sends every span beat to the witness with the tree root at that beat and a
consistency proof from the root the witness last signed. The witness remembers the new root first
and countersigns second, so a chain rewritten at or before any position it countersigned is refused
on the next push and fails verification against the witness's own statement.

The witness URL must be https. Plain http is accepted only to a loopback address, 127.0.0.0/8,
::1, or localhost, since over the network anyone on the path could answer for the witness.

Enrollment needs a license that includes the hosted witness: Enterprise, or Team with the add-on.
The request is signed by this install's producer key and carries the license file, and the witness's
answer is signed by the witness key. The command prints that key's id. Compare it with the id the
witness publishes at /.well-known/loomseal.json and in docs/witness.md before trusting what it
countersigns, or pass --witness-key to refuse any other.

The enrollment is written to witness-enrollment.json beside the license file, so a backup and a
restore carry both or neither. It holds no secret: every push is authenticated by the producer
signature. Enrolling again answers the same. A server started with --span-cadence pushes its beats
once the file exists.

--refresh-keys fetches the witness's published key list after it rotates its key and keeps it only
when a key the enrollment already holds signed it.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runWitnessEnroll,
}

// init registers the enroll command and its flags.
func init() {
	witnessEnrollCmd.Flags().StringVar(&witnessEnrollDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN, of the install to enroll.")
	witnessEnrollCmd.Flags().StringVar(&witnessEnrollURL, "witness", "",
		"Base URL of the witness to enroll with. Required unless --refresh-keys is given.")
	witnessEnrollCmd.Flags().StringVar(&witnessEnrollKey, "witness-key", "",
		"Witness key id, sha256:..., the witness must sign with. Anything else is refused and "+
			"nothing is written.")
	witnessEnrollCmd.Flags().BoolVar(&witnessEnrollRefresh, "refresh-keys", false,
		"Fetch the enrolled witness's key list and keep it only when a key the enrollment already "+
			"holds signed it.")
	witnessCmd.AddCommand(witnessEnrollCmd)
}

// runWitnessEnroll gates on the license, then enrolls the install or refreshes the witness keys.
func runWitnessEnroll(cmd *cobra.Command, _ []string) error {
	licensePath := license.PathFor(witnessEnrollDB)
	licenseFile, err := gateHostedWitness(licensePath)
	if err != nil {
		return err
	}
	pin := strings.TrimSpace(witnessEnrollKey)
	if pin != "" && !witnessKeyIDForm.MatchString(pin) {
		return fmt.Errorf("--witness-key %q is not a witness key id; it is sha256: and 64 lowercase "+
			"hex characters, as the witness publishes it", witnessEnrollKey)
	}
	path := witnessclient.PathFor(licensePath)
	ctx, cancel := context.WithTimeout(cmd.Context(), witnessEnrollTimeout)
	defer cancel()
	if witnessEnrollRefresh {
		return refreshWitnessKeys(ctx, cmd, path, pin)
	}
	if strings.TrimSpace(witnessEnrollURL) == "" {
		return fmt.Errorf("--witness is required: name the base URL of the witness to enroll with")
	}
	base, err := witnessclient.NormalizeURL(witnessEnrollURL)
	if err != nil {
		return err
	}
	if err := witnessclient.RequireSecureURL(base); err != nil {
		return fmt.Errorf("%w. Nothing was sent", err)
	}
	held, err := witnessclient.LoadIfPresent(path)
	if err != nil {
		return fmt.Errorf("%w. Remove it to enroll from scratch", err)
	}
	if held != nil && held.Witness != base {
		return fmt.Errorf("this install is enrolled with the witness at %s, not %s. Remove %s to "+
			"enroll with another witness", held.Witness, base, path)
	}

	store, err := openExisting(witnessEnrollDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()
	id, err := loadProducerIdentity(cmd.Context(), store.Audits(), witnessEnrollDB)
	if err != nil {
		return err
	}
	enr, err := witnessclient.Enroll(ctx, nil, base, id, licenseFile, time.Now())
	if err != nil {
		return err
	}
	if pin != "" {
		if err := enr.Pin(pin); err != nil {
			return err
		}
	}
	if err := enr.Follows(held); err != nil {
		return err
	}
	// Recorded before the file is written, so a change the chain cannot hold is not made. Enrolling
	// again with the same answer changes nothing and records nothing.
	if !enr.Same(held) {
		if err := recordWitnessEnrollment(cmd.Context(), store.Audits(), "/cli/witness/enroll",
			enr); err != nil {
			return err
		}
		if err := enr.Save(path); err != nil {
			return err
		}
	}
	out, err := jsonutil.Marshal(map[string]any{
		"witness": enr.Witness, "witness_key_id": enr.Keys.KeyID, "install_id": enr.InstallID,
		"key_id": enr.KeyID, "enrolled_at": enr.EnrolledAt, "file": path,
	}, false)
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(out))
	errOut := cmd.ErrOrStderr()
	fmt.Fprintf(errOut, "Enrolled install %s, producer key %s, with the witness at %s.\n",
		enr.InstallID, enr.KeyID, enr.Witness)
	if pin != "" {
		fmt.Fprintf(errOut, "The witness signs with %s, the pinned key.\n", enr.Keys.KeyID)
	} else {
		fmt.Fprintf(errOut, "The witness signs with %s. Compare it with the key id published at "+
			"%s%s and in docs/witness.md before trusting what it countersigns, or pass "+
			"--witness-key to pin it.\n", enr.Keys.KeyID, enr.Witness, witnessclient.KeysPath)
	}
	fmt.Fprintf(errOut, "Wrote %s. A server started with --span-cadence pushes its beats to the "+
		"witness from its next start.\n", path)
	return nil
}

// gateHostedWitness reads the license file at licensePath, installs it as the process's license,
// and refuses unless it includes the hosted witness. It returns the file's bytes, which the
// enrollment carries for the witness's own check. A file that does not verify is refused rather
// than read as Community, since the witness would refuse it too.
func gateHostedWitness(licensePath string) ([]byte, error) {
	raw, err := os.ReadFile(licensePath)
	var lic *license.License
	switch {
	case errors.Is(err, fs.ErrNotExist):
		raw = nil
	case err != nil:
		return nil, fmt.Errorf("read license: %w", err)
	default:
		if lic, err = license.Verify(raw, licensePath); err != nil {
			return nil, fmt.Errorf("the license at %s does not verify, so this install runs "+
				"Community: %w", licensePath, err)
		}
	}
	license.Set(lic)
	if err := license.Allow(license.FeatureHostedWitness); err != nil {
		return nil, err
	}
	return raw, nil
}

// refreshWitnessKeys fetches the enrolled witness's key list and keeps it when a held key signed
// it, printing what was kept.
func refreshWitnessKeys(ctx context.Context, cmd *cobra.Command, path, pin string) error {
	held, err := witnessclient.Load(path)
	if err != nil {
		return fmt.Errorf("%w. Enroll with switchtender witness enroll --witness <url> first", err)
	}
	if err := witnessclient.RequireSecureURL(held.Witness); err != nil {
		return fmt.Errorf("%w. Enroll again with the witness's https URL. Nothing was sent", err)
	}
	if witnessEnrollURL != "" {
		base, err := witnessclient.NormalizeURL(witnessEnrollURL)
		if err != nil {
			return err
		}
		if base != held.Witness {
			return fmt.Errorf("this install is enrolled with the witness at %s, not %s", held.Witness,
				base)
		}
	}
	enr, changed, err := witnessclient.RefreshKeys(ctx, nil, path, pin)
	if err != nil {
		return err
	}
	if changed {
		store, err := openExisting(witnessEnrollDB)
		if err != nil {
			return fmt.Errorf("open store: %w", err)
		}
		defer func() { _ = store.Close() }()
		if err := recordWitnessEnrollment(cmd.Context(), store.Audits(),
			"/cli/witness/refresh-keys", enr); err != nil {
			return err
		}
		if err := enr.Save(path); err != nil {
			return err
		}
	}
	out, err := jsonutil.Marshal(map[string]any{
		"witness": enr.Witness, "witness_key_id": enr.Keys.KeyID, "keys": len(enr.Keys.Keys),
		"changed": changed, "file": path,
	}, false)
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(out))
	if !changed {
		fmt.Fprintf(cmd.ErrOrStderr(), "The witness key list is unchanged: the witness signs with "+
			"%s.\n", enr.Keys.KeyID)
		return nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Kept the witness's new key list, signed by %s, which this "+
		"install held: the witness now signs with %s. A running server takes it the next time the "+
		"witness answers with that key.\n", held.Keys.KeyID, enr.Keys.KeyID)
	return nil
}

// witnessEnrollmentChange is what an enrollment entry's content digest commits to: the witness the
// install is enrolled with, the witness key it trusts, and the producer key and label the witness
// bound. None of it is secret.
type witnessEnrollmentChange struct {
	// Witness is the witness's base URL.
	Witness string `json:"witness"`
	// WitnessKeyID is the witness key the enrollment names as current.
	WitnessKeyID string `json:"witness_key_id"`
	// WitnessKeys counts the keys the witness's list names, retired ones included.
	WitnessKeys int `json:"witness_keys"`
	// InstallID is the install's label.
	InstallID string `json:"install_id"`
	// KeyID is the producer key the witness keeps the install's memory under.
	KeyID string `json:"key_id"`
	// EnrolledAt is when the witness first enrolled the install.
	EnrolledAt string `json:"enrolled_at"`
}

// recordWitnessEnrollment records an enrollment, or a refreshed witness key list, in the audit
// chain with its summary committed, so the chain says which outside witness this install trusts and
// from when.
func recordWitnessEnrollment(ctx context.Context, audits audit.Store, path string,
	enr *witnessclient.Enrollment) error {
	body, err := json.Marshal(witnessEnrollmentChange{Witness: enr.Witness,
		WitnessKeyID: enr.Keys.KeyID, WitnessKeys: len(enr.Keys.Keys), InstallID: enr.InstallID,
		KeyID: enr.KeyID, EnrolledAt: enr.EnrolledAt})
	if err != nil {
		return fmt.Errorf("encode the enrollment change: %w", err)
	}
	return recordCLIChange(ctx, audits, witnessEnrollDB, path, body)
}
