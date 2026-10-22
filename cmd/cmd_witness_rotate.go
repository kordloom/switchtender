package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/jsonutil"
	"github.com/kordloom/switchtender/witness"
)

var (
	// witnessRotateKeyDir is the key directory whose current key rotate-key retires.
	witnessRotateKeyDir string
	// witnessRotateStateDir is the witness's state directory, which the key directory must stay
	// apart from.
	witnessRotateStateDir string
)

// witnessRotateCmd retires the witness signing key and makes a new one current.
var witnessRotateCmd = &cobra.Command{
	Use:   "rotate-key",
	Short: "Retire the witness signing key and make a new one current.",
	Long: `Retire the witness signing key and make a new one current.

Stop the witness first, rotate, then start it again: a running witness keeps signing with the key it
loaded. The retired key stays in the key directory, owner-only, and the witness's published key list
names every key it has signed with, each with when it began signing and when it retired. The list is
signed by the new key and endorsed by every retired key the directory still holds, so an enrolled
install that holds an earlier key accepts the new list on that key's word. Attestations an earlier
key made before it retired verify against the list for as long as anyone keeps them. Deleting a
retired key's file stops it endorsing anything again, which is the step after a compromise, and the
installs that held only that key then pin the new one by hand.

The key directory must be apart from the state directory: not the same directory, not inside it, and
not holding it. The state directory is copied off the host, and the keys must not travel with it.
Back the key directory up as a whole after a rotation, since the history and the retired keys are
part of what the witness publishes.

Prints the retired key and the new current key as JSON.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runWitnessRotate,
}

// init registers the rotate-key command and its flags.
func init() {
	witnessRotateCmd.Flags().StringVar(&witnessRotateKeyDir, "key-dir", "",
		"Directory holding the witness signing key, the one witness serve runs with. Required, and "+
			"it must be apart from the state directory.")
	witnessRotateCmd.Flags().StringVar(&witnessRotateStateDir, "state-dir", "switchtender-witness",
		"The witness state directory, the one witness serve runs with. The key directory must not "+
			"be it, hold it, or lie inside it.")
	witnessCmd.AddCommand(witnessRotateCmd)
}

// runWitnessRotate rotates the key in the key directory and prints the two keys.
func runWitnessRotate(cmd *cobra.Command, _ []string) error {
	if strings.TrimSpace(witnessRotateKeyDir) == "" {
		return fmt.Errorf("--key-dir is required: retired keys stay in the key directory, which " +
			"must be apart from the state directory that is copied off the host")
	}
	r, err := witness.RotateKey(witnessRotateKeyDir, witnessRotateStateDir, time.Now())
	if err != nil {
		return err
	}
	out, err := jsonutil.Marshal(r, true)
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(out))
	fmt.Fprintf(cmd.ErrOrStderr(), "witness: retired %s, now signing with %s; start the witness "+
		"again so it signs with the new key\n", r.Retired.KeyID, r.Current.KeyID)
	return nil
}
