package migration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// upgBinaryEnv names a previous release's switchtender binary to run instead of building one.
	upgBinaryEnv = "SWITCHTENDER_PREVIOUS_RELEASE_BINARY"
	// upgRepoEnv names the repository checkout the previous release is built from, for a tree that
	// is not itself a checkout.
	upgRepoEnv = "SWITCHTENDER_PREVIOUS_RELEASE_REPO"
	// upgRefEnv names the git ref the previous release is built from, in place of the release tag
	// found below the version the chart says this tree builds.
	upgRefEnv = "SWITCHTENDER_PREVIOUS_RELEASE_REF"
)

// upgPrevious is the previous release's binary, built once per test process, or why it could not
// be.
var upgPrevious struct {
	// once guards the build.
	once sync.Once
	// path is the binary.
	path string
	// skip is why no previous release is available on a machine allowed to lack one.
	skip string
	// fail is why the previous release could not be built where it is present.
	fail string
}

// upgPreviousRelease returns the switchtender binary of the release this tree upgrades from, built
// from the repository's own release tag: the newest one below the version the chart says this tree
// builds, which is the rule the supertest's upgrade phase picks its starting release by. A machine
// with no checkout or no tags stands the scenario down, unless the full suite is demanded.
func upgPreviousRelease(t *testing.T) string {
	t.Helper()
	upgPrevious.once.Do(upgBuildPrevious)
	switch {
	case upgPrevious.fail != "":
		t.Fatal(upgPrevious.fail)
	case upgPrevious.skip != "":
		standDownOrFail(t, "%s", upgPrevious.skip)
	}
	return upgPrevious.path
}

// upgBuildPrevious finds the previous release and builds it, recording the binary or why there is
// none. The source is exported with git archive, so no checkout of any repository is changed.
func upgBuildPrevious() {
	if bin := os.Getenv(upgBinaryEnv); bin != "" {
		upgPrevious.path = bin
		return
	}
	repo := os.Getenv(upgRepoEnv)
	if repo == "" {
		out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
		if err != nil {
			upgPrevious.skip = "this tree is not a git checkout, so the previous release cannot be " +
				"built; set " + upgBinaryEnv + " or " + upgRepoEnv
			return
		}
		repo = strings.TrimSpace(string(out))
	}
	ref := os.Getenv(upgRefEnv)
	if ref == "" {
		var err error
		if ref, err = upgPreviousTag(repo); err != nil {
			upgPrevious.skip = err.Error()
			return
		}
	}
	dir, err := os.MkdirTemp("", "stmig-previous-")
	if err != nil {
		upgPrevious.fail = "create the previous release's directory: " + err.Error()
		return
	}
	archive := filepath.Join(dir, "release.tar")
	if out, err := exec.Command("git", "-C", repo, "archive", "--format=tar", "-o", archive,
		ref).CombinedOutput(); err != nil {
		upgPrevious.fail = "export the previous release " + ref + ": " + err.Error() + "\n" + string(out)
		return
	}
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		upgPrevious.fail = "create the previous release's source directory: " + err.Error()
		return
	}
	if out, err := exec.Command("tar", "-xf", archive, "-C", src).CombinedOutput(); err != nil {
		upgPrevious.fail = "unpack the previous release " + ref + ": " + err.Error() + "\n" + string(out)
		return
	}
	bin := filepath.Join(dir, "switchtender")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = src
	if out, err := build.CombinedOutput(); err != nil {
		upgPrevious.fail = "build the previous release " + ref + ": " + err.Error() + "\n" + string(out)
		return
	}
	upgPrevious.path = bin
}

// upgChartVersion reads the version the chart says a tree builds.
var upgChartVersion = regexp.MustCompile(`(?m)^appVersion:\s*"?([0-9][0-9.]*)"?`)

// upgPreviousTag returns the release tag of the newest release below the version the chart in repo
// says it builds, preferring a v tag to the snapshot tag of the same release.
func upgPreviousTag(repo string) (string, error) {
	chart, err := os.ReadFile(filepath.Join(repo, "deploy", "helm", "switchtender", "Chart.yaml"))
	if err != nil {
		return "", fmt.Errorf("read the version the chart builds: %w", err)
	}
	m := upgChartVersion.FindSubmatch(chart)
	if m == nil {
		return "", errors.New("the chart names no appVersion, so no previous release can be chosen")
	}
	building := string(m[1])
	out, err := exec.Command("git", "-C", repo, "tag", "--list", "v*", "snapshot/v*").Output()
	if err != nil {
		return "", fmt.Errorf("list the release tags: %w", err)
	}
	refs := map[string]string{}
	for _, tag := range strings.Fields(string(out)) {
		v := strings.TrimPrefix(strings.TrimPrefix(tag, "snapshot/"), "v")
		if _, seen := refs[v]; !seen || !strings.HasPrefix(tag, "snapshot/") {
			refs[v] = tag
		}
	}
	best := ""
	for v := range refs {
		if upgLessVersion(v, building) && (best == "" || upgLessVersion(best, v)) {
			best = v
		}
	}
	if best == "" {
		return "", fmt.Errorf("no release tag is older than %s, which the chart says this tree "+
			"builds; fetch tags or set %s", building, upgBinaryEnv)
	}
	return refs[best], nil
}

// upgLessVersion reports whether dotted version a orders before b, field by field as numbers.
func upgLessVersion(a, b string) bool {
	fa, fb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(fa) || i < len(fb); i++ {
		na, nb := upgVersionField(fa, i), upgVersionField(fb, i)
		if na != nb {
			return na < nb
		}
	}
	return false
}

// upgVersionField returns field i of a dotted version as a number, zero when it is absent or is
// not one.
func upgVersionField(fields []string, i int) int {
	if i >= len(fields) {
		return 0
	}
	n, err := strconv.Atoi(fields[i])
	if err != nil {
		return 0
	}
	return n
}

// upgStartRelease starts bin, another release's switchtender binary, as a server on the install
// and waits until it is ready, the way startServer starts this tree's.
func (in *install) upgStartRelease(bin, name string) *server {
	in.t.Helper()
	port := freePort(in.t)
	args := []string{"serve", "--db", in.db, "--addr", "127.0.0.1:" + port,
		"--forward-state", filepath.Join(in.root, "forward-"+name+".json")}
	if in.policyPath != "" {
		args = append(args, "--policy-file", in.policyPath)
	}
	logPath := filepath.Join(in.root, "logs", fmt.Sprintf("%s-%d.log", name, len(in.servers)))
	logFile, err := os.Create(logPath)
	if err != nil {
		in.t.Fatalf("create the server log: %v", err)
	}
	c := exec.Command(bin, args...)
	c.Env = in.env
	c.Dir = in.root
	c.Stdout, c.Stderr = logFile, logFile
	if err := c.Start(); err != nil {
		in.t.Fatalf("start server %s: %v", name, err)
	}
	s := &server{name: name, url: "http://127.0.0.1:" + port, cmd: c, logPath: logPath,
		done: make(chan struct{})}
	go func() {
		s.exitErr = c.Wait()
		_ = logFile.Close()
		close(s.done)
	}()
	in.servers = append(in.servers, s)
	in.t.Cleanup(func() { s.stop() })
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			log, _ := os.ReadFile(logPath)
			in.t.Fatalf("server %s exited before it was ready: %v\n%s", name, s.exitErr, log)
		default:
		}
		res, err := http.Get(s.url + "/readyz")
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return s
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	log, _ := os.ReadFile(logPath)
	in.t.Fatalf("server %s was not ready within %s:\n%s", name, waitLimit, log)
	return nil
}

// upgRelease runs one command of another release's binary on the install and returns its combined
// output and error.
func (in *install) upgRelease(bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	c := exec.CommandContext(ctx, bin, args...)
	c.Env = in.env
	c.Dir = in.root
	out, err := c.CombinedOutput()
	return string(out), err
}

// upgCount reads the count a backup or restore summary gives for label, or -1 when it gives none.
func upgCount(summary, label string) int {
	m := regexp.MustCompile(`(?:^|[\s,])` + regexp.QuoteMeta(label) + ` (\d+)`).
		FindStringSubmatch(summary)
	if m == nil {
		return -1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return -1
	}
	return n
}

// upgTemplate returns the template named name as the list serves it.
func (in *install) upgTemplate(s *server, name string) map[string]any {
	in.t.Helper()
	var page struct {
		// Templates are every template, as the list serves them.
		Templates []map[string]any `json:"templates"`
	}
	in.must(s, "admin", "GET", "/v1/templates", nil, 200).decode(in.t, &page)
	for _, tpl := range page.Templates {
		if tpl["name"] == name {
			return tpl
		}
	}
	in.t.Fatalf("template %q is not in the list", name)
	return nil
}

// TestDowngradePreviousReleaseLeavesAParkedWorkflowAlone puts the release this tree upgrades from
// on a database where this release has parked a workflow at an approval step. On PostgreSQL both
// releases serve the database at once, which is what the chart's default rolling update does for
// the length of a rollout. On SQLite the previous release replaces this one, which is a rollback,
// the path the supertest proves and the FAQ promises.
//
// A parked workflow is stored as a pipeline run in pending_approval with no lease, which is exactly
// what a whole workflow held before it starts looks like to every earlier release. The previous
// release lists it as a run awaiting approval, and an approver who releases it there starts the
// workflow over from its first step: the build that already ran runs a second time, the approval
// step runs as a step with no playbook and fails, the deny path runs although nobody denied
// anything, and the real approval step is left waiting under a workflow that has ended. No step may
// run twice and no path may be taken without a decision, whichever release serves the request.
func TestDowngradePreviousReleaseLeavesAParkedWorkflowAlone(t *testing.T) {
	t.Parallel()
	previous := upgPreviousRelease(t)
	for _, store := range []storeKind{onSQLite, onPostgres} {
		t.Run(string(store), func(t *testing.T) {
			t.Parallel()
			in := newInstall(t, installOptions{Store: store})
			current := in.startServer("current")
			wf := in.launched(current, "operator", "release", nil)
			in.waitPending(current, "operator", wf.ID)
			in.requireSteps(current, wf.ID, map[string]int{"build": 1, "ship": 0, "page": 0})
			if store == onSQLite {
				// SQLite holds one server, so going back to the previous release replaces this one.
				current.stop()
			}
			prev := in.upgStartRelease(previous, "previous")

			approve := in.api(prev, "approver", "POST", "/v1/runs/"+wf.ID+"/approve",
				map[string]any{})
			state := ""
			if approve.Status == http.StatusOK {
				state = in.waitDone(prev, wf.ID).Status
			} else {
				// Two janitor ticks, so a release that acts on the parked workflow on its own has
				// had the chance to.
				time.Sleep(12 * time.Second)
				state = in.getRun(prev, wf.ID).Status
			}
			if got := in.executions("build"); got != 1 {
				t.Errorf("the previous release answered %d to an approval of the parked workflow and "+
					"its build step ran %d times, want once: a finished step ran again", approve.Status,
					got)
			}
			if got := in.executions("page"); got != 0 {
				t.Errorf("the deny path ran %d times though nobody denied the approval step", got)
			}
			if terminal(state) {
				t.Errorf("the parked workflow ended %s without its approval step being decided", state)
			}
		})
	}
}

// TestDowngradePreviousReleaseRefusesABackupItCannotRead takes a backup with this release and
// restores it with the release this tree upgrades from, the order a rollback followed by a
// recovery from the latest backup takes.
//
// The file carries notification targets, their attachments, a template's sealed provisioning
// callback key, and a secret survey question's sealed default, none of which the previous release
// knows. It is still stamped with the payload version the previous release reads, so that release
// decodes it, drops all four, and reports a complete restore. The backup format promises that a
// file at a different version is refused rather than misread, so the previous release has to refuse
// this file, or restore everything it holds.
func TestDowngradePreviousReleaseRefusesABackupItCannotRead(t *testing.T) {
	t.Parallel()
	previous := upgPreviousRelease(t)
	in := newInstall(t, installOptions{Store: onSQLite})
	s := in.startServer("current")
	in.setSecretDefault(s, "rotate db password", "db_password", "survey-default-"+randomHex(t, 8))
	before := in.upgTemplate(s, "provision")
	if before["allow_callbacks"] != true || before["host_config_key_set"] != true {
		t.Fatalf("the imported provision template carries no callback key to lose: %s",
			describe(before))
	}
	s.stop()

	file := filepath.Join(in.root, "current.stbak")
	took := in.cli("backup", "--db", in.db, "--out", file)
	if upgCount(took, "notification targets") != 1 || upgCount(took, "notification attachments") != 2 {
		t.Fatalf("the backup does not hold the notification target the scenario needs:\n%s", took)
	}

	restored := filepath.Join(in.root, "db", "restored.db")
	out, err := in.upgRelease(previous, "restore", "--db", restored, "--in", file)
	if err != nil {
		// Refused, which is what a file the previous release cannot fully read has to get.
		return
	}

	// The restore reported success. Read what it restored with this release, as the operator does
	// when the upgrade is applied again.
	again := in.cli("backup", "--db", restored, "--out", filepath.Join(in.root, "restored.stbak"))
	var lost []string
	if got := upgCount(again, "notification targets"); got != 1 {
		lost = append(lost, fmt.Sprintf("notification targets: %d of 1", got))
	}
	if got := upgCount(again, "notification attachments"); got != 2 {
		lost = append(lost, fmt.Sprintf("notification attachments: %d of 2", got))
	}
	in.db = restored
	r := in.startServer("reopened")
	if after := in.upgTemplate(r, "provision"); after["allow_callbacks"] != true ||
		after["host_config_key_set"] != true {
		lost = append(lost, "the provision template's callbacks and callback key")
	}
	survey := in.upgTemplate(r, "rotate db password")["survey"]
	if !strings.Contains(describe(survey), `"default"`) {
		lost = append(lost, "the secret survey question's default")
	}
	if len(lost) > 0 {
		t.Errorf("the previous release reported a complete restore of a backup it could not read, "+
			"and dropped %s:\n%s", strings.Join(lost, "; "), out)
	}
}

// TestUpgradeToPostgresKeepsProvisioningCallbacks moves an install from SQLite to PostgreSQL the
// way the backup page documents as the growth path, backup then restore, and has a host call back
// on the imported provision template there.
//
// The callback handler deduplicates a host's callbacks across replicas through a run idempotency
// key it builds by joining the template id and the host with a NUL byte. PostgreSQL text cannot
// hold a NUL, so the dedupe lookup fails and every provisioning callback on PostgreSQL answers 500,
// on exactly the backend whose replicas the dedupe exists for. SQLite stores the byte, which is why
// the callback scenario, run on SQLite alone, passes.
func TestUpgradeToPostgresKeepsProvisioningCallbacks(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onSQLite})
	file := filepath.Join(in.root, "move.stbak")
	in.cli("backup", "--db", in.db, "--out", file)
	in.db = ownDatabase(t)
	in.cli("restore", "--db", in.db, "--in", file)
	s := in.startServer("postgres", "--trusted-proxy", "127.0.0.1/32")
	tpl := in.template(s, "provision")

	r := in.apiHeaders(s, "", "POST", "/v1/templates/"+tpl+"/callback",
		map[string]any{"host_config_key": hostConfigKey},
		map[string]string{"X-Forwarded-For": "10.20.0.12"})
	if r.Status != http.StatusCreated {
		log, _ := os.ReadFile(s.logPath)
		why := ""
		for _, line := range strings.Split(string(log), "\n") {
			if strings.Contains(line, "provisioning callback") {
				why = line
				break
			}
		}
		t.Fatalf("a host's callback on PostgreSQL = %d, want 201: %s\n%s", r.Status, r.Body,
			upgClip(why, 400))
	}
}

// upgClip shortens s to at most n bytes for a failure message.
func upgClip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
