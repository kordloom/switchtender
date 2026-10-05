package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/jsonutil"
)

// workerKeyOut holds the value of the worker key new --out flag.
var workerKeyOut string

// workerKeyCmd groups the commands that make and read a relay worker pool's delivery key.
var workerKeyCmd = &cobra.Command{
	Use:   "key",
	Short: "Make and read the delivery keys relay worker pools receive sealed run secrets with.",
	Args:  cobra.NoArgs,
	RunE:  runGroupHelp,
}

// workerKeyNewCmd generates a delivery key, writes its private half to a file, and prints its
// public half for the worker pool file.
var workerKeyNewCmd = &cobra.Command{
	Use:   "new",
	Short: "Generate a delivery key: write the private key to a file and print the public key.",
	Long: "Generate a delivery key for a relay worker pool. The private key is written to --out,\n" +
		"mode 0600, and never over an existing file. Install it on every worker of the pool and\n" +
		"start each with --delivery-key. The public key is printed: register it as the pool's\n" +
		"delivery_key in the worker pool file the control node reads with --worker-pools.",
	Args: cobra.NoArgs,
	RunE: runWorkerKeyNew,
}

// workerKeyPublicCmd prints the public half of an existing delivery key file.
var workerKeyPublicCmd = &cobra.Command{
	Use:   "public <key-file>",
	Short: "Print the public key and key id of a delivery key file.",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkerKeyPublic,
}

// init registers the worker key commands and flags.
func init() {
	workerKeyNewCmd.Flags().StringVar(&workerKeyOut, "out", "",
		"File to write the new private key to, mode 0600. Required. An existing file is never "+
			"overwritten.")
	workerKeyCmd.AddCommand(workerKeyNewCmd, workerKeyPublicCmd)
	workerCmd.AddCommand(workerKeyCmd)
}

// deliveryKeyInfo is what the key commands print: everything public about a delivery key.
type deliveryKeyInfo struct {
	// KeyID names the key in the audit chain and in a worker's log.
	KeyID string `json:"key_id"`
	// DeliveryKey is the public key, the value the pool's delivery_key takes.
	DeliveryKey string `json:"delivery_key"`
	// File is where the private key was written, set by key new only.
	File string `json:"file,omitempty"`
}

// runWorkerKeyNew generates a key, writes the private half to --out, and prints the public half.
func runWorkerKeyNew(cmd *cobra.Command, _ []string) error {
	if workerKeyOut == "" {
		return fmt.Errorf("%w: --out names the file the private key is written to", ErrUsage)
	}
	k, err := handoff.GenerateKey()
	if err != nil {
		return err
	}
	if err := handoff.WriteKeyFile(workerKeyOut, k); err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "wrote the private key to %s. Register the public key as the "+
		"pool's delivery_key in the worker pool file.\n", workerKeyOut)
	return printKeyInfo(cmd, deliveryKeyInfo{KeyID: k.ID(), DeliveryKey: k.Public().String(),
		File: workerKeyOut})
}

// runWorkerKeyPublic prints the public half of a delivery key file, refusing one another account
// can read, the same check a worker applies before it uses the key.
func runWorkerKeyPublic(cmd *cobra.Command, args []string) error {
	k, err := handoff.LoadPrivateKeyFile(args[0])
	if err != nil {
		return err
	}
	return printKeyInfo(cmd, deliveryKeyInfo{KeyID: k.ID(), DeliveryKey: k.Public().String()})
}

// printKeyInfo writes info to stdout as one JSON line.
func printKeyInfo(cmd *cobra.Command, info deliveryKeyInfo) error {
	data, err := jsonutil.Marshal(info, false)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
	return err
}
