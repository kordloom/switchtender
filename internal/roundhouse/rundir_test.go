//go:build unix

package roundhouse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// privateRunDir makes a run-files root and one run directory in it, both 0700, the shape the
// dispatcher hands a runner.
func privateRunDir(t *testing.T, base string) (root, dir string) {
	t.Helper()
	root = filepath.Join(base, "runfiles")
	dir = filepath.Join(root, "run-r1-123")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root, dir
}

// runLine returns the arguments of the stub runtime's run invocation.
func runLine(t *testing.T, dir string) string {
	t.Helper()
	for _, line := range strings.Split(readStubFile(t, dir, "calls.log"), "\n") {
		if strings.HasPrefix(line, "run ") {
			return line
		}
	}
	t.Fatalf("the runtime was never asked to run anything:\n%s", readStubFile(t, dir, "calls.log"))
	return ""
}

// TestContainerRunStagesSecretsInTheRunDirectory proves a containerized run keeps everything secret
// it writes in its private directory: the environment file that holds every injected credential,
// the extra vars file, the script, and the registry login the runtime writes. Inside the container
// that directory is an in-memory filesystem, sized, nosuid and nodev, with exec allowed. Without
// the run directory the same files land in the shared temporary directory, where a crash leaves
// them to whatever clears it.
func TestContainerRunStagesSecretsInTheRunDirectory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Spec is the run.
		Spec Spec
		// WantStaged are the name prefixes of files that must be mounted from the run directory.
		WantStaged []string
	}{{ // Test 0: A playbook with secret answers, credentials, and a private image.
		Spec: Spec{Tool: run.ToolAnsible, Playbook: "site.yml", ExtraVars: map[string]any{"db": "pw"},
			Env: []string{"AWS_ACCESS_KEY_ID=AKIA"}, RegistryUsername: "ci", RegistryPassword: "hunter2"},
		WantStaged: []string{"switchtender-vars-", "switchtender-env-"},
	}, { // Test 1: A script that carries a secret inline.
		Spec: Spec{Tool: run.ToolBash, Command: "echo token=abc", Env: []string{"KUBECONFIG=/k"},
			RegistryUsername: "ci", RegistryPassword: "hunter2"},
		WantStaged: []string{"switchtender-sh-", "switchtender-env-"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			bin, stub := stubRuntimeExits(t, 0, 0)
			c := newContainerRunner(bin, "missing", false, stubBaseEnv(), &pluginCache{},
				DefaultContainerLimits())
			root, dir := privateRunDir(t, t.TempDir())
			spec := test.Spec
			spec.Image, spec.Dir = "ghcr.io/org/private:1", t.TempDir()
			if spec.Playbook != "" {
				spec.Playbook = filepath.Join(spec.Dir, spec.Playbook)
			}
			spec.RunDir, spec.RunFilesRoot = dir, root
			if _, err := c.Run(context.Background(), spec, io.Discard); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			line := runLine(t, stub)
			tmpfs := "--tmpfs " + dir + ":rw,exec,nosuid,nodev,size=64m,mode=0700,uid=" +
				strconv.Itoa(os.Getuid()) + ",gid=" + strconv.Itoa(os.Getgid())
			if !strings.Contains(line, tmpfs) {
				t.Errorf("run arguments lack %q:\n%s", tmpfs, line)
			}
			for _, prefix := range test.WantStaged {
				if !strings.Contains(line, "-v "+filepath.Join(dir, prefix)) {
					t.Errorf("%s was not mounted from the run directory %s:\n%s", prefix, dir, line)
				}
			}
			login := readStubFile(t, stub, "login.env")
			if !strings.Contains(login, "DOCKER_CONFIG="+filepath.Join(dir, "switchtender-registry-")) {
				t.Errorf("the registry login was not written in the run directory:\n%s", login)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("the run left %d entries in its directory, want them removed", len(entries))
			}
		})
	}
}

// TestHostRunnersStageScriptsInTheRunDirectory proves a host run writes its script and its extra
// vars file inside the run directory and removes them when it ends.
func TestHostRunnersStageScriptsInTheRunDirectory(t *testing.T) {
	t.Parallel()
	_, dir := privateRunDir(t, t.TempDir())
	var out bytes.Buffer
	r := newBashRunner(stubBaseEnv())
	script := Spec{Command: `echo "$0"`, RunDir: dir}
	if _, err := r.Run(context.Background(), script, &out); err != nil {
		t.Fatalf("bash Run() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); filepath.Dir(got) != dir {
		t.Errorf("the script ran from %s, want inside the run directory %s", got, dir)
	}

	stub := filepath.Join(t.TempDir(), "ansible-playbook")
	writeStub(t, stub, "#!/bin/sh\n[ \"$1\" = stub-warmup ] && exit 0\necho \"$@\"\n")
	a := newAnsibleRunner(WithBinary(stub))
	out.Reset()
	spec := Spec{Playbook: "site.yml", ExtraVars: map[string]any{"db_password": "pw"}, RunDir: dir}
	if _, err := a.Run(context.Background(), spec, &out); err != nil {
		t.Fatalf("ansible Run() error = %v", err)
	}
	if !strings.Contains(out.String(), "@"+filepath.Join(dir, "switchtender-vars-")) {
		t.Errorf("the extra vars file was not in the run directory: %s", out.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the runs left %d entries in the run directory, want them removed", len(entries))
	}
}

// TestMountGuardLetsTheRunFilesRootThrough proves the exception the run-files root needs and no
// more. A file staged under the root is mountable even when the root sits in a tree the guard
// refuses, as a systemd runtime directory under /run does, here stood in for by a directory with a
// credential store's name. A link out of the root, a socket, the root itself, a path outside it,
// and a root open to other accounts all stay refused.
func TestMountGuardLetsTheRunFilesRootThrough(t *testing.T) {
	t.Parallel()
	base := filepath.Join(t.TempDir(), ".docker")
	root, dir := privateRunDir(t, base)
	staged := filepath.Join(dir, "cred-1")
	if err := os.WriteFile(staged, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("/etc/hosts", link); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "agent.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	openBase := filepath.Join(t.TempDir(), ".docker")
	openRoot, openDir := privateRunDir(t, openBase)
	if err := os.Chmod(openRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	openStaged := filepath.Join(openDir, "cred-1")
	if err := os.WriteFile(openStaged, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		// Private is the run-files root the mount set knows.
		Private string
		// Path is the host path to mount.
		Path string
		// Want is the error.
		Want error
	}{{ // Test 0: Without the root known, the credential store's name refuses the staged file.
		Private: "", Path: staged, Want: ErrForbiddenMount,
	}, { // Test 1: With the root known, the staged file mounts.
		Private: root, Path: staged, Want: nil,
	}, { // Test 2: A link out of the root to a host file stays refused.
		Private: root, Path: link, Want: ErrForbiddenMount,
	}, { // Test 3: A socket stays refused.
		Private: root, Path: sock, Want: ErrForbiddenMount,
	}, { // Test 4: The root itself is not something a run mounts.
		Private: root, Path: root, Want: ErrForbiddenMount,
	}, { // Test 5: A path beside the root is not inside it.
		Private: root, Path: outside, Want: ErrForbiddenMount,
	}, { // Test 6: A root other accounts can read is not private, so it earns no exception.
		Private: openRoot, Path: openStaged, Want: ErrForbiddenMount,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			m := newMountSet()
			m.private = test.Private
			if err := m.add(test.Path, true); !errors.Is(err, test.Want) {
				t.Errorf("add(%s) with root %q error = %v, want %v", test.Path, test.Private, err, test.Want)
			}
		})
	}
}

// TestSecretsMountRefusesAPathItCannotMount proves a run directory a container cannot mount at, a
// Windows path or one carrying the colon the runtime splits on, refuses the run rather than
// starting a container whose secrets would sit somewhere else.
func TestSecretsMountRefusesAPathItCannotMount(t *testing.T) {
	t.Parallel()
	c := newContainerRunner("docker", "missing", false, nil, &pluginCache{},
		ContainerLimits{RunFilesSize: "8m"})
	for _, dir := range []string{`C:\Users\op\run-1`, "relative/run-1", "/tmp/a:b/run-1"} {
		if _, err := c.secretsMount(dir); err == nil {
			t.Errorf("secretsMount(%q) = nil error, want a refusal", dir)
		}
	}
	got, err := c.secretsMount("/run/switchtender/runfiles/run-1")
	want := "/run/switchtender/runfiles/run-1:rw,exec,nosuid,nodev,size=8m,"
	if err != nil || !strings.HasPrefix(got, want) {
		t.Errorf("secretsMount() = %q, %v, want the configured size", got, err)
	}
	if strings.Contains(got, "noexec") {
		t.Errorf("secretsMount() = %q, want exec allowed", got)
	}
}
