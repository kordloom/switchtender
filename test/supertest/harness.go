package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// harness holds everything the phases share: where the cluster's kubeconfig lives, where the
// working files go, which local port the API is forwarded to, and the ledger of checks.
//
// Nothing here touches state outside its own working directory. The cluster gets its own name,
// kubectl and helm are always pointed at a private kubeconfig rather than the user's, and teardown
// removes all of it. A test that leaves footprints on the machine that ran it is a test nobody
// runs twice.
type harness struct {
	// repo is the repository root, where the chart, the Dockerfile, and the module live.
	repo string
	// work is the scratch directory for keys, kubeconfig, receipts, and the built binary.
	work string
	// cluster is the Kind cluster name.
	cluster string
	// kubeconfig is the private kubeconfig path every kubectl and helm call uses.
	kubeconfig string
	// bin is the locally built switchtender binary, used to verify receipts offline. Verification
	// deliberately runs outside the cluster: evidence a server checks about itself proves nothing.
	bin string
	// portSeq numbers local forward ports so every forward binds a port nothing has touched.
	portSeq int
	// skipTeam records that the Team tier was not exercised, so the report says what ran rather
	// than what usually runs.
	skipTeam bool
	// api is the base URL of whichever install is currently port-forwarded.
	api string
	// forward is the running port-forward process, ended before the next one starts.
	forward *exec.Cmd
	// checks is the ledger every phase writes into and the report is rendered from.
	checks []check
	// ids remembers objects the phases created, keyed by a name of this package's choosing, so a
	// later phase can reference what an earlier one made without threading values everywhere.
	ids map[string]string
	// human and agent are the two actors the phases act as, minted during bootstrap. A person
	// approves; an agent proposes and is refused its own approval. Both carry a real token.
	human actor
	agent actor
	// seed is the shared signing identity the Team install runs with, minted per run.
	seed string
	// httpc is the one HTTP client every call shares. A timeout is not optional: kubectl
	// port-forward has a known half-dead state where the listener accepts and the stream stalls,
	// and a client without a deadline turns that into a run that hangs forever, skips teardown,
	// and leaks the cluster.
	httpc *http.Client
	// created is set once this run has actually created its Kind cluster, and is what entitles
	// teardown to delete it. Without it, a run that failed before creating anything deleted
	// whatever cluster happened to share the name, including one kept deliberately for debugging.
	created bool
	// keep leaves the cluster running after the run, for poking at a failure.
	keep bool
}

// bootstrap adopts the initial admin token the server minted on first boot, then uses it to create
// the two accounts the phases act as and to mint each one a token of its own.
//
// A fresh public install with no tokens mints an admin token and prints it once to its own logs,
// which is exactly the handover a real operator picks up. The harness reads it from the server
// pod's log rather than being handed a token out of band: adopting the real first credential is
// more faithful than minting one past the gate, and it proves the bootstrap path itself works.
func (h *harness) bootstrap(namespace string) error {
	// The initial admin token prints on whichever server pod won first-boot initialization, so
	// every server pod's logs are scanned: an active-active install has two, and reading only the
	// first found the loser's logs about half the time.
	pods, err := h.kubectl("get", "pod", "-n", namespace,
		"-l", "app.kubernetes.io/component=server", "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return err
	}
	names := strings.Fields(pods)
	if len(names) == 0 {
		return fmt.Errorf("no server pod found in %s", namespace)
	}
	adminToken := ""
	var allLogs strings.Builder
	for _, pod := range names {
		logs, lerr := h.kubectl("logs", "-n", namespace, pod)
		if lerr != nil {
			return fmt.Errorf("read server logs for the initial token: %w", lerr)
		}
		allLogs.WriteString(logs)
		if adminToken == "" {
			adminToken = parseInitialToken(logs)
		}
	}
	if adminToken == "" {
		return fmt.Errorf("no server pod printed an initial admin token; logs:\n%s",
			allLogs.String())
	}
	admin := &actor{Name: "initial", Type: "user", Token: adminToken}

	for _, account := range []struct {
		Username string
		Role     string
	}{
		{"casey", "admin"},
		{"release-agent", "operator"},
	} {
		err := h.apiCall("POST", "/v1/users", admin, map[string]any{
			"username": account.Username,
			"password": "supertest-not-a-secret",
			"role":     account.Role,
		}, nil)
		if err != nil {
			return fmt.Errorf("create account %s: %w", account.Username, err)
		}
	}

	var human, agent struct {
		Token string `json:"token"`
	}
	if err := h.apiCall("POST", "/v1/tokens", admin, map[string]any{
		"name": "supertest-human", "username": "casey",
	}, &human); err != nil {
		return fmt.Errorf("mint human token: %w", err)
	}
	if err := h.apiCall("POST", "/v1/tokens", admin, map[string]any{
		"name": "supertest-agent", "username": "release-agent", "kind": "agent",
	}, &agent); err != nil {
		return fmt.Errorf("mint agent token: %w", err)
	}
	h.human = actor{Name: "casey", Type: "user", Token: human.Token}
	h.agent = actor{Name: "release-agent", Type: "agent", Token: agent.Token}
	return nil
}

// parseInitialToken pulls the one-time admin token out of the server's boot log. The banner prints
// it indented on its own line between the "shown only this once" notice and the usage lines.
func parseInitialToken(logs string) string {
	marker := "shown only this once:"
	i := strings.Index(logs, marker)
	if i < 0 {
		return ""
	}
	for _, line := range strings.Split(logs[i+len(marker):], "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// check is one verified claim: what was asserted, whether it held, and the evidence.
type check struct {
	// Phase groups the check under the phase that made it.
	Phase string
	// Name states the claim in plain words.
	Name string
	// Err is nil when the claim held.
	Err error
	// Detail carries the observed evidence worth showing either way.
	Detail string
}

// pass records a claim that held, with the evidence that showed it.
func (h *harness) pass(phase, name, detail string) {
	h.checks = append(h.checks, check{Phase: phase, Name: name, Detail: detail})
	fmt.Printf("   ok   %s\n", name)
	if detail != "" {
		fmt.Printf("        %s\n", detail)
	}
}

// fail records a claim that did not hold. The run continues, because the report is worth more
// complete than truncated at the first failure.
func (h *harness) fail(phase, name string, err error) {
	h.checks = append(h.checks, check{Phase: phase, Name: name, Err: err})
	fmt.Printf("   FAIL %s\n        %v\n", name, err)
}

// failed counts the checks that did not hold.
func (h *harness) failed() int {
	n := 0
	for _, c := range h.checks {
		if c.Err != nil {
			n++
		}
	}
	return n
}

// run executes a command, returning its combined output, with the private kubeconfig in its
// environment so kubectl and helm never read the user's.
func (h *harness) run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+h.kubeconfig)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// runIn is run with stdin supplied, which is how playbooks reach the server pod and manifests
// reach kubectl without touching the filesystem inside the cluster.
func (h *harness) runIn(stdin string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+h.kubeconfig)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// runOut runs a command and returns stdout alone, with stderr carried only inside the error. The
// ordinary helpers combine the streams, which is right for forensics and wrong for capture: a
// sealed backup taken through the combined helper came back with the human count report stitched
// into the envelope bytes.
func (h *harness) runOut(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+h.kubeconfig)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err,
			stderr.String())
	}
	return stdout.String(), nil
}

// writePodFile lands content at path inside a pod and proves it landed whole. It carries the
// bytes on the argv as base64 rather than on stdin, because kubectl exec -i can drop stdin while
// the far command exits zero: cat wrote an empty playbook about half the time under load, ansible
// answered "Empty playbook, nothing to do", and the flake wore the face of a product failure. The
// byte count is verified after the write, so a landing that was not whole is an error here, named,
// rather than a mystery two phases later.
func (h *harness) writePodFile(namespace, pod, path, content string) error {
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	script := fmt.Sprintf("printf %%s %s | base64 -d > %s && wc -c < %s", encoded, path, path)
	out, err := h.kubectl("exec", "-n", namespace, pod, "--", "sh", "-c", script)
	if err != nil {
		return fmt.Errorf("write %s into %s: %w", path, pod, err)
	}
	got := strings.TrimSpace(out)
	want := fmt.Sprintf("%d", len(content))
	if got != want {
		return fmt.Errorf("write %s into %s: %s bytes landed, want %s", path, pod, got, want)
	}
	return nil
}

// kubectl runs kubectl against the supertest cluster.
func (h *harness) kubectl(args ...string) (string, error) {
	return h.run("kubectl", args...)
}

// apply feeds a manifest to kubectl apply.
func (h *harness) apply(manifest string) error {
	_, err := h.runIn(manifest, "kubectl", "apply", "-f", "-")
	return err
}

// forwardTo ends any current port-forward and starts one to the named service, waiting until the
// API answers. Every call gets a fresh local port, never a reused one: a port that just carried a
// killed forward is a bind race the kernel sometimes loses, and losing it looked like sixty
// seconds of connection refused with nothing to blame. Fresh ports also keep the original
// property, that a stale forward can never answer for the wrong install.
func (h *harness) forwardTo(namespace, service string) error {
	return h.forwardTarget(namespace, "svc/"+service)
}

// forwardToPod forwards to one named pod, for the moments where WHICH endpoint answers is the
// claim itself: proving the surviving replica serves alone means dialing the survivor by name,
// because a service-routed forward can resolve to the dying pod's sandbox and burn the whole
// window on an endpoint that no longer exists.
func (h *harness) forwardToPod(namespace, pod string) error {
	return h.forwardTarget(namespace, "pod/"+pod)
}

// forwardTarget ends any current port-forward and starts one to the named target, waiting until
// the API answers.
func (h *harness) forwardTarget(namespace, target string) error {
	h.stopForward()
	h.portSeq++
	localPort := 18900 + h.portSeq
	cmd := exec.Command("kubectl", "port-forward", "-n", namespace, target,
		fmt.Sprintf("%d:8080", localPort))
	cmd.Env = append(os.Environ(), "KUBECONFIG="+h.kubeconfig)
	// The child's stderr is kept: a forward that dies at birth used to fail as sixty seconds of
	// connection refused with the actual reason discarded, which is the least useful failure
	// there is.
	var forwardErr bytes.Buffer
	cmd.Stderr = &forwardErr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start port-forward: %w", err)
	}
	h.forward = cmd
	h.api = fmt.Sprintf("http://127.0.0.1:%d", localPort)
	err := h.waitFor(fmt.Sprintf("%s/healthz answers", h.api), 60*time.Second, func() error {
		resp, err := h.httpc.Get(h.api + "/healthz")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("healthz answered %d", resp.StatusCode)
		}
		return nil
	})
	if err != nil && forwardErr.Len() > 0 {
		return fmt.Errorf("%w; kubectl port-forward said: %s", err, forwardErr.String())
	}
	return err
}

// stopForward ends the current port-forward, if one is running.
func (h *harness) stopForward() {
	if h.forward != nil && h.forward.Process != nil {
		_ = h.forward.Process.Kill()
		_ = h.forward.Wait()
	}
	h.forward = nil
}

// waitFor polls until the condition holds or the deadline passes, and says what it was waiting
// for when it gives up, because "timeout" with no subject is the least useful failure there is.
func (h *harness) waitFor(what string, limit time.Duration, cond func() error) error {
	deadline := time.Now().Add(limit)
	var last error
	for time.Now().Before(deadline) {
		if last = cond(); last == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("gave up waiting for %s after %s: %w", what, limit, last)
}

// actor names who a request comes from and carries the bearer token that authenticates it.
//
// The identity headers are hints the gate reads once a token has authenticated the request; on an
// enforcing install they authenticate nothing on their own. A real caller holds a token, so the
// harness holds one too rather than pretending headers are credentials.
type actor struct {
	// Name is the acting identity, echoed on the identity header for the record.
	Name string
	// Type is user or agent, which is what separation-of-duties decisions read.
	Type string
	// Token is the plaintext bearer token minted for this actor, empty before bootstrap and on the
	// endpoints reachable during it.
	Token string
}

// api sends one JSON request to the forwarded install and decodes the response into out when out
// is non-nil. A non-2xx status is an error carrying the body, so a refusal shows its sentence.
func (h *harness) apiCall(method, path string, who *actor, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, h.api+path, reader)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if who != nil {
		req.Header.Set("X-Switchtender-Actor", who.Name)
		req.Header.Set("X-Switchtender-Actor-Type", who.Type)
		if who.Token != "" {
			req.Header.Set("Authorization", "Bearer "+who.Token)
		}
	}
	resp, err := h.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &apiError{Method: method, Path: path, Status: resp.StatusCode,
			Body: strings.TrimSpace(string(raw))}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

// apiError is a non-2xx answer, carrying the status so a check can tell a refusal from a crash.
// When every error was one opaque string, a 500, a 404, and a dropped connection all counted as
// proof that a security gate held, which is the exact opposite of proof.
type apiError struct {
	// Method and Path name the request.
	Method, Path string
	// Status is the HTTP status the server answered.
	Status int
	// Body is the response body, which for a refusal is the sentence worth showing.
	Body string
}

// Error renders the answer the way the checks report it.
func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s answered %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// mustRefuse sends a request that the authorization model must refuse, and accepts nothing except
// an actual ruling: 401 or 403. Success means the gate is open; any other status or a transport
// failure means the gate was never tested, and a check that treats either as a pass certifies
// separation of duties it never exercised.
func (h *harness) mustRefuse(who *actor, method, path string, body any) (string, error) {
	err := h.apiCall(method, path, who, body, nil)
	if err == nil {
		return "", fmt.Errorf("%s %s was allowed", method, path)
	}
	var api *apiError
	if !errors.As(err, &api) {
		return "", fmt.Errorf("the gate was never reached: %w", err)
	}
	if api.Status != http.StatusUnauthorized && api.Status != http.StatusForbidden {
		return "", fmt.Errorf("answered %d, which is a malfunction rather than a refusal: %s",
			api.Status, api.Body)
	}
	return oneLine(api.Error()), nil
}

// auditFingerprint derives the sha256: fingerprint of the public key behind this run's signing
// seed, entirely locally: hex seed to ed25519 key to hashed public key, the same derivation the
// product's own trust page performs. Pinning it on verification is what makes the receipt checks
// mean "signed by the identity this run configured" rather than "signed by whoever signed it".
func (h *harness) auditFingerprint() string {
	raw, err := hex.DecodeString(h.auditSeed())
	if err != nil {
		panic(err)
	}
	pub := ed25519.NewKeyFromSeed(raw).Public().(ed25519.PublicKey)
	sum := sha256.Sum256(pub)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// serverPod returns the name of the single server pod in a namespace, discovered rather than
// assumed, so a renamed release cannot silently point every exec at nothing.
func (h *harness) serverPod(namespace string) (string, error) {
	out, err := h.kubectl("get", "pod", "-n", namespace,
		"-l", "app.kubernetes.io/component=server",
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("no server pod found in %s", namespace)
	}
	return strings.TrimSpace(out), nil
}

// serviceName returns the one Service the chart created in a namespace, discovered the same way.
func (h *harness) serviceName(namespace string) (string, error) {
	out, err := h.kubectl("get", "svc", "-n", namespace,
		"-l", "app.kubernetes.io/name=switchtender",
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("no switchtender service found in %s", namespace)
	}
	return strings.TrimSpace(out), nil
}

// installForensics gathers what a failed install looked like from the cluster's side: every pod,
// its state, and the server's own log tail. An install failure that reports only "not ready" makes
// somebody re-run the whole thing with their hands on kubectl; this does that first pass for them.
func (h *harness) installForensics(namespace string) string {
	pods, _ := h.kubectl("get", "pods", "-n", namespace, "-o", "wide")
	events, _ := h.kubectl("get", "events", "-n", namespace,
		"--sort-by=.lastTimestamp")
	var logs string
	if pod, err := h.serverPod(namespace); err == nil {
		logs, _ = h.kubectl("logs", "-n", namespace, pod, "--tail=40")
	}
	sections := []string{"pods:\n" + pods}
	if events != "" {
		lines := strings.Split(strings.TrimRight(events, "\n"), "\n")
		if len(lines) > 15 {
			lines = lines[len(lines)-15:]
		}
		sections = append(sections, "events:\n"+strings.Join(lines, "\n"))
	}
	if logs != "" {
		sections = append(sections, "server log tail:\n"+logs)
	}
	return strings.Join(sections, "\n")
}

// fleetExec runs a command on one fleet machine through kubectl exec, which is the outside path:
// what it reads owes nothing to SwitchTender's own reporting.
func (h *harness) fleetExec(pod string, args ...string) (string, error) {
	full := append([]string{"exec", "-n", "fleet", pod, "--"}, args...)
	out, err := h.kubectl(full...)
	return strings.TrimSpace(out), err
}

// auditSeed returns the hex seed this run's Team install signs with, minting it once. It is the
// identity a real operator generates with openssl rand -hex 32 and holds in a secret manager.
func (h *harness) auditSeed() string {
	if h.seed == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			panic(err)
		}
		h.seed = hex.EncodeToString(raw)
	}
	return h.seed
}
