package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/util"
	"github.com/kordloom/switchtender/witness"
)

var (
	// witnessServer is the watched server's base URL.
	witnessServer string
	// witnessState is where the signed checkpoint lives.
	witnessState string
	// witnessInterval is how often the loop polls.
	witnessInterval time.Duration
	// witnessOnce runs one check and exits, for cron.
	witnessOnce bool
	// witnessKeyDir overrides where the witness signing identity lives.
	witnessKeyDir string
	// witnessWebhook, when set, receives each finding as a JSON POST.
	witnessWebhook string
)

// witnessCmd watches a server's span beat feed from outside it.
var witnessCmd = &cobra.Command{
	Use:   "witness",
	Short: "Watch a server's beat feed from outside it and attest to what was seen.",
	Long: `Watch a server's span beat feed from outside it.

A chain proves what it holds was not altered. It cannot prove nothing was removed from the end,
because the process that runs the chain also decides what gets written down. A witness on another
machine remembers what the feed served, keeps that memory in a signed checkpoint file, and raises
a finding when a beat goes missing, an already-witnessed beat comes back rewritten, or the head
regresses. Run it where the server's operator has no hand.

Findings go to standard error and, with --webhook, to your channel as a JSON POST. Exit is
nonzero under --once when the check found problems, so a cron line can page on it.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runWitness,
}

// init registers the witness command and its flags.
func init() {
	witnessCmd.Flags().StringVar(&witnessServer, "server", "",
		"Base URL of the SwitchTender server to watch. Required.")
	witnessCmd.Flags().StringVar(&witnessState, "state", witness.DefaultStatePath(),
		"Signed checkpoint file holding what this witness has seen.")
	witnessCmd.Flags().DurationVar(&witnessInterval, "interval", time.Minute,
		"How often to poll the feed.")
	witnessCmd.Flags().BoolVar(&witnessOnce, "once", false,
		"Run one check and exit nonzero on findings, for cron.")
	witnessCmd.Flags().StringVar(&witnessKeyDir, "key-dir", "",
		"Directory holding the witness signing key. Defaults to the state file's directory.")
	witnessCmd.Flags().StringVar(&witnessWebhook, "webhook", "",
		"URL that receives each finding as a JSON POST.")
	rootCmd.AddCommand(witnessCmd)
}

// runWitness polls the feed until stopped, or once under --once.
func runWitness(cmd *cobra.Command, _ []string) error {
	if witnessServer == "" {
		return fmt.Errorf("--server is required")
	}
	base, err := url.Parse(witnessServer)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return fmt.Errorf("--server must be an http or https URL")
	}
	keyDir := witnessKeyDir
	if keyDir == "" {
		keyDir = filepath.Dir(witnessState)
	}
	id, err := audit.LoadWitnessIdentity(keyDir)
	if err != nil {
		return fmt.Errorf("load witness identity: %w", err)
	}
	witnessServer = witness.NormalizeServer(witnessServer)
	fmt.Fprintf(os.Stderr, "witness: watching %s, checkpoint %s, key %s\n",
		witnessServer, witnessState, id.KeyID())

	client := &http.Client{Timeout: 30 * time.Second}
	watcher := witness.NewWatcher(witnessServer, witnessState, id, client)
	// The outage edge pages once when checking starts failing and once when it recovers. A page
	// every minute all night for one unreachable server trains the reader to mute the channel the
	// real finding arrives on.
	var edge witness.BlindEdge
	// In loop mode a persisting condition is reported when it appears, not once per poll; the
	// repeat delivery an operator mutes is the channel the next real finding arrives on.
	var delta witness.Delta
	// Findings the webhook has not accepted yet. The plain witness has no durable record behind
	// it, so an alert lost to one failed POST would be an alert lost for good; it is retried each
	// poll until the channel takes it.
	var undelivered []witness.Finding
	for {
		cp, findings, err := watcher.CheckOnce(cmd.Context())
		if witnessOnce {
			printFindings(findings)
			for _, f := range findings {
				// Once mode has no retry queue behind it, so a lost POST is lost for good. It cannot
				// be silent: the cron that runs --once triages by the channel, and a discarded error
				// would leave the finding in the channel's silence. Report it on stderr so the run's
				// output names the delivery failure even though it cannot retry it.
				if derr := postWitnessFinding(cmd.Context(), client, witnessWebhook, watcher.Server(), f); derr != nil {
					fmt.Fprintf(os.Stderr, "witness: webhook delivery failed for %s finding: %v\n", f.Kind, derr)
				}
			}
			if err != nil {
				return err
			}
			if len(findings) > 0 {
				return fmt.Errorf("the witness raised %d %s, each printed above", len(findings),
					util.Plural(len(findings), "finding", "findings"))
			}
			fmt.Fprintln(os.Stderr, witnessSummary(cp))
			return nil
		}
		if err == nil || len(findings) > 0 {
			// A poll that produced findings observed the chain, even when saving the checkpoint
			// then failed, so it is folded rather than re-reported every poll.
			findings = delta.Fresh(findings)
		}
		printFindings(findings)
		undelivered = append(undelivered, findings...)
		if err != nil {
			// A witness that cannot check is not a witness. Logging to a stream nobody reads and
			// looping forever is the failure mode where the process is up, the operator believes
			// they are covered, and a truncation goes unobserved, so the channel hears about it.
			fmt.Fprintln(os.Stderr, "witness: "+err.Error())
		}
		if f := edge.Observe(err); f != nil {
			undelivered = append(undelivered, *f)
		}
		undelivered = deliverFindings(cmd.Context(), client, watcher.Server(), undelivered)
		select {
		case <-cmd.Context().Done():
			return nil
		case <-time.After(witnessInterval):
		}
	}
}

// undeliveredCap bounds the retry queue. Past it the oldest alerts are dropped with a loud line,
// because a webhook that has been down for hundreds of findings is an operator problem the queue
// cannot fix by growing forever.
const undeliveredCap = 100

// printFindings writes each finding to standard error.
func printFindings(findings []witness.Finding) {
	for _, f := range findings {
		fmt.Fprintf(os.Stderr, "witness: FINDING %s: %s\n", f.Kind, f.Detail)
	}
}

// deliverFindings posts queued findings to the webhook in order, returning what still awaits
// delivery. The first failure stops the round so ordering holds, and without a webhook there is
// nothing to wait for.
func deliverFindings(ctx context.Context, client *http.Client, server string,
	queue []witness.Finding) []witness.Finding {
	if witnessWebhook == "" || len(queue) == 0 {
		return nil
	}
	for i, f := range queue {
		if err := postWitnessFinding(ctx, client, witnessWebhook, server, f); err != nil {
			fmt.Fprintf(os.Stderr, "witness: webhook: %v; %d %s queued for redelivery\n",
				err, len(queue)-i, util.Plural(len(queue)-i, "finding", "findings"))
			rest := queue[i:]
			if len(rest) > undeliveredCap {
				dropped := len(rest) - undeliveredCap
				fmt.Fprintf(os.Stderr, "witness: webhook: dropping %d oldest undelivered %s\n",
					dropped, util.Plural(dropped, "finding", "findings"))
				rest = rest[len(rest)-undeliveredCap:]
			}
			return rest
		}
	}
	return nil
}

// postWitnessFinding posts one finding to the operator's channel. A transport failure or a
// non-2xx answer is returned, because a delivery the channel refused is a delivery that did not
// happen, whatever the transport says.
func postWitnessFinding(ctx context.Context, client *http.Client, webhook, server string, f witness.Finding) error {
	if webhook == "" {
		return nil
	}
	body := map[string]string{
		"server": server, "kind": f.Kind, "detail": f.Detail,
		"at": time.Now().UTC().Format(time.RFC3339),
	}
	if f.Class != "" {
		body["class"] = f.Class
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return postWitnessBody(ctx, client, webhook, payload)
}

// postWitnessBody posts one JSON body to a channel. A transport failure or a non-2xx answer is
// returned, because a delivery the channel refused is a delivery that did not happen, whatever the
// transport says. The generic webhook and Slack both post through it.
func postWitnessBody(ctx context.Context, client *http.Client, target string, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("the webhook answered %s", res.Status)
	}
	return nil
}

// The witness posts findings to Slack from one goroutine of its own. The service hands a finding to
// its notifier before it answers the push that raised it, so a post made there would hold the
// install's answer for as long as Slack took, a retry included.
const (
	// witnessSlackRetryWait is how long a Slack post that failed waits before its one retry.
	witnessSlackRetryWait = 2 * time.Second
	// witnessSlackQueue bounds the messages waiting for Slack. Past it a message is dropped with a
	// log line, since the findings record is the durable copy and a push never waits on Slack.
	witnessSlackQueue = 256
)

// witnessSlackText renders one finding as the Slack message the witness posts. A finding about an
// enrolled install names its class and the producer key id the witness keeps the install's record
// under. A finding about a watched server has no class and names the server.
func witnessSlackText(subject string, f witness.Finding) string {
	if f.Class == "" {
		return fmt.Sprintf("SwitchTender witness: %s on watched server %s\n> %s", f.Kind, subject,
			f.Detail)
	}
	return fmt.Sprintf("SwitchTender witness %s: %s for producer key %s\n> %s", f.Class, f.Kind,
		subject, f.Detail)
}

// postWitnessSlack posts one message to a Slack incoming webhook, and once more after wait when the
// first attempt fails, so a passing fault at Slack does not cost the alert. It returns the last
// attempt's error with the webhook URL masked out of it, because the URL is the credential.
func postWitnessSlack(ctx context.Context, client *http.Client, webhook, text string, wait time.Duration) error {
	payload, err := dispatch.SlackBody(text)
	if err != nil {
		return err
	}
	if err = postWitnessBody(ctx, client, webhook, payload); err == nil {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return util.MaskURLError(err, webhook)
	}
	return util.MaskURLError(postWitnessBody(ctx, client, webhook, payload), webhook)
}

// witnessSlack delivers findings to Slack incoming webhooks in the order they were raised, from the
// goroutine that runs it.
type witnessSlack struct {
	// urls are the incoming webhook URLs every finding is posted to.
	urls []string
	// client posts the messages.
	client *http.Client
	// wait is how long a failed post waits before its one retry.
	wait time.Duration
	// log receives every delivery that failed twice and every dropped message, with no URL unmasked.
	log *zap.Logger
	// queue holds the messages waiting for delivery, oldest first.
	queue chan string
}

// newWitnessSlack returns a Slack notifier for urls. Nothing is posted until run is started.
func newWitnessSlack(urls []string, client *http.Client, wait time.Duration, log *zap.Logger) *witnessSlack {
	return &witnessSlack{urls: urls, client: client, wait: wait, log: log,
		queue: make(chan string, witnessSlackQueue)}
}

// notify queues one finding for Slack and returns at once.
func (n *witnessSlack) notify(subject string, f witness.Finding) {
	select {
	case n.queue <- witnessSlackText(subject, f):
	default:
		n.log.Warn("witness: slack queue full, dropping a finding",
			zap.String("subject", subject), zap.String("finding", f.Kind))
	}
}

// run posts queued messages to every webhook until ctx ends.
func (n *witnessSlack) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case text := <-n.queue:
			for _, webhook := range n.urls {
				if err := postWitnessSlack(ctx, n.client, webhook, text, n.wait); err != nil {
					n.log.Warn("witness: slack delivery failed",
						zap.String("url", util.MaskURL(webhook)), zap.Error(err))
				}
			}
		}
	}
}

// witnessSummary is the line a clean --once check ends with: what this witness now holds. Without
// it a first run printed only what it was watching, so a clean check and one that never looked read
// the same, and a later run could not show how far the record had moved.
func witnessSummary(cp *witness.Checkpoint) string {
	if cp == nil {
		return "witness: no findings."
	}
	head := cp.LastHead
	if len(head) > 12 {
		head = head[:12]
	}
	return fmt.Sprintf("witness: no findings. Checkpoint at beat %d, chain position %d, head %s.",
		cp.LastBeat, cp.LastSeq, head)
}
