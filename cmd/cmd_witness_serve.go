package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/jsonutil"
	"github.com/kordloom/switchtender/internal/logutil"
	"github.com/kordloom/switchtender/internal/witness"
)

var (
	// witnessWatch lists the servers the hosted witness watches.
	witnessWatch []string
	// witnessStateDir holds the checkpoints and the findings record.
	witnessStateDir string
	// witnessListen is where the witness API serves.
	witnessListen string
	// witnessServeInterval is how often every watched server is checked.
	witnessServeInterval time.Duration
	// witnessServeKeyDir overrides where the witness signing identity lives.
	witnessServeKeyDir string
	// witnessServeWebhook receives every finding as a JSON POST.
	witnessServeWebhook string
)

// witnessServeCmd runs the hosted witness: many servers watched from one process, with an API.
var witnessServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Watch many servers from one process and answer auditors with countersigned attestations.",
	Long: `Run the hosted witness.

One process watches every server given with --watch, keeps a signed checkpoint per server, records
every finding durably, and serves what it has witnessed over HTTP:

  GET /witness/servers                       every watched server and its status
  GET /witness/servers/{key}/checkpoint      the signed memory for one server
  GET /witness/servers/{key}/attestation     a freshly countersigned statement of what this
                                             witness holds: the head it saw, when, and how many
                                             findings it has ever recorded
  GET /witness/servers/{key}/findings        that server's recorded findings
  GET /witness/findings                      every recorded finding

Independence is the point. Run this where the watched servers' operators have no hand: another
team, another company, or a service. Publish the witness key id, and a relying party verifies any
attestation offline with "switchtender witness verify-attestation" against the key they pinned.
The watched operator cannot mint one, cannot alter one, and cannot answer one that disagrees with
the chain they serve.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runWitnessServe,
}

// witnessVerifyCmd verifies an attestation offline, for the relying party.
var witnessVerifyCmd = &cobra.Command{
	Use:   "verify-attestation <attestation.json>",
	Short: "Verify a witness attestation offline against a pinned witness key.",
	Long: `Verify a witness attestation offline.

Reads a JSON attestation, recomputes its signature, and prints the verdict with the signer's key.
Pass --pubkey with the witness key you have pinned; without it the check proves only that the
document is internally consistent, which a forger with their own key satisfies trivially.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runWitnessVerify,
}

// witnessVerifyPubkey is the pinned witness key an attestation must be signed by.
var witnessVerifyPubkey string

// init registers the hosted witness commands and their flags.
func init() {
	witnessServeCmd.Flags().StringArrayVar(&witnessWatch, "watch", nil,
		"Base URL of a SwitchTender server to watch. Repeatable, at least one.")
	witnessServeCmd.Flags().StringVar(&witnessStateDir, "state-dir", "switchtender-witness",
		"Directory holding the signed checkpoints and the findings record.")
	witnessServeCmd.Flags().StringVar(&witnessListen, "listen", "127.0.0.1:9440",
		"Address the witness API serves on.")
	witnessServeCmd.Flags().DurationVar(&witnessServeInterval, "interval", time.Minute,
		"How often every watched server is checked. At least 10s.")
	witnessServeCmd.Flags().StringVar(&witnessServeKeyDir, "key-dir", "",
		"Directory holding the witness signing key. Defaults to the state directory.")
	witnessServeCmd.Flags().StringVar(&witnessServeWebhook, "webhook", "",
		"URL that receives every finding as a JSON POST.")
	witnessVerifyCmd.Flags().StringVar(&witnessVerifyPubkey, "pubkey", "",
		"Pinned witness public key in hex; verification fails when the signer differs.")
	witnessCmd.AddCommand(witnessServeCmd, witnessVerifyCmd)
}

// runWitnessServe validates the watch list, starts the service, and serves its API until stopped.
func runWitnessServe(cmd *cobra.Command, _ []string) error {
	if len(witnessWatch) == 0 {
		return fmt.Errorf("--watch is required at least once")
	}
	for _, raw := range witnessWatch {
		base, err := url.Parse(raw)
		if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
			return fmt.Errorf("--watch %q must be an http or https URL", raw)
		}
	}
	if witnessServeInterval < 10*time.Second {
		return fmt.Errorf("--interval must be at least 10s, got %s", witnessServeInterval)
	}
	keyDir := witnessServeKeyDir
	if keyDir == "" {
		keyDir = witnessStateDir
	}
	if err := os.MkdirAll(keyDir, 0o750); err != nil {
		return fmt.Errorf("witness key directory: %w", err)
	}
	id, err := audit.LoadIdentity(keyDir)
	if err != nil {
		return fmt.Errorf("load witness identity: %w", err)
	}

	// The signal context is established first so Ctrl+C and SIGTERM actually reach the shutdown
	// branch; cobra runs commands with a background context that no signal ever cancels.
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log, err := logutil.New()
	if err != nil {
		return fmt.Errorf("logger: %w", err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	var opts []witness.ServiceOption
	if witnessServeWebhook != "" {
		opts = append(opts, witness.WithServiceNotify(func(server string, f witness.Finding) {
			// The service's findings record is the durable copy, so a lost POST does not lose the
			// finding from the API. But the operator wired a webhook to be paged on tampering, so a
			// delivery failure must be visible rather than swallowed: log it. postWitnessFinding does
			// not log on its own, and the earlier comment that claimed it did was wrong.
			if derr := postWitnessFinding(ctx, client, witnessServeWebhook, server, f); derr != nil {
				log.Warn("witness: webhook delivery failed",
					zap.String("server", server), zap.String("finding", f.Kind), zap.Error(derr))
			}
		}))
	}
	svc, err := witness.NewService(id, witnessStateDir, witnessServeInterval, witnessWatch,
		log, client, opts...)
	if err != nil {
		return err
	}
	if err := svc.Start(); err != nil {
		return err
	}
	defer svc.Close()

	fmt.Fprintf(os.Stderr, "witness: watching %d server(s), state %s, key %s\n",
		len(witnessWatch), witnessStateDir, id.KeyID())
	fmt.Fprintf(os.Stderr, "witness: api on %s; publish the key id so relying parties can pin it\n",
		witnessListen)

	httpSrv := &http.Server{
		Addr: witnessListen, Handler: svc.Handler(), ReadHeaderTimeout: 10 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.ListenAndServe() }()
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		return nil
	}
}

// runWitnessVerify checks one attestation and prints the verdict.
func runWitnessVerify(_ *cobra.Command, args []string) error {
	data, err := os.ReadFile(args[0])
	if err != nil {
		return fmt.Errorf("read attestation: %w", err)
	}
	var a witness.Attestation
	if err := json.Unmarshal(data, &a); err != nil {
		return fmt.Errorf("parse attestation: %w", err)
	}
	signer, err := witness.VerifyAttestation(&a)
	verdict := map[string]any{
		"ok":             err == nil,
		"server":         a.Server,
		"minted_at":      a.MintedAt,
		"last_beat":      a.LastBeat,
		"last_seq":       a.LastSeq,
		"last_head":      a.LastHead,
		"findings_total": a.FindingsTotal,
		"blind":          a.Blind,
		"signed_by":      signer,
	}
	if err != nil {
		verdict["problem"] = err.Error()
	}
	// The pin is what turns a self-consistent document into one from the witness you trust.
	if err == nil && witnessVerifyPubkey != "" && signer != witnessVerifyPubkey {
		verdict["ok"] = false
		verdict["problem"] = "signed by " + signer + ", not by the pinned witness key"
	}
	out, jerr := jsonutil.Marshal(verdict, true)
	if jerr != nil {
		return jerr
	}
	fmt.Println(string(out))
	if verdict["ok"] != true {
		return fmt.Errorf("attestation did not verify")
	}
	return nil
}
