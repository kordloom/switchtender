# Run files

Many tools take a secret only as a file, so a run writes some of its secrets to disk while it
executes. This page covers where they go, how they are removed on every exit path and after a crash,
and what keeps them off persistent disk.

## What a run stages

SwitchTender writes these for a run, and only for as long as the run needs them:

- SSH private keys, unlocked in memory first so a passphrase never reaches disk.
- Ansible vault password files, and the vars files that carry become, connection, and network
  device passwords off the command line.
- The extra vars file, which carries secret survey answers.
- An inline script for Bash, Python, Go, or PowerShell, which can carry whatever was typed into it.
- Files rendered by credential types, such as cloud configs and kubeconfigs.
- Federated identity token files, and the external account file Google Cloud reads over one.
- The stored inventory the run targets, which can carry secret host variables.
- For a containerized run, the environment file every injected value is sourced from, and the
  registry login the container runtime writes when it pulls a private image.
- The run's copy of the fact cache.

That is SwitchTender-managed staging. The tools a run starts write their own state as well, and the
cloud command line tools write state about the credentials they were handed. That part is covered
under [Tool state](#tool-state).

## Where run files live

Each run gets one private directory, mode 0700, with every file in it mode 0600. The directories
live under a root that belongs to the account SwitchTender runs as, mode 0700. The root is chosen in
this order:

1. `--runfiles-dir`, or `SWITCHTENDER_RUNFILES_DIR` when the flag is absent.
2. The systemd runtime directory, when the process runs as a unit that sets `RuntimeDirectory=`. It
   is memory-backed under `/run`, private to the service, and removed by systemd when the service
   stops for any reason. The root is `runfiles` inside it.
3. The account's `XDG_RUNTIME_DIR`, but only when that directory is owned by the account and closed
   to everyone else. The root is `switchtender-runfiles` inside it.
4. Otherwise the temporary directory, as `switchtender-runfiles-<uid>`, and the doctor, at
   `/ui/doctor` and `GET /v1/doctor`, warns about it.

SwitchTender never picks `/dev/shm`, and refuses it when named: every process on the host shares it,
and a container sizes it for shared memory.

Before a server or worker serves, it proves the root:

- The root is a real directory owned by the account, not a symbolic link, and is closed to 0700.
  Every directory above it is owned by the account or the superuser, and none is writable by other
  accounts unless it has the sticky bit, as `/tmp` does. Otherwise another account could swap the
  root for a directory of its own.
- The root is on a filesystem known to be local. A network filesystem is refused, since a lock there
  can be lost or held by another machine, and a sweep that misread a live run as dead would delete
  its credentials mid-job. A filesystem SwitchTender does not know is refused as well, even if its
  locks happen to work.
- A probe takes a lock in the root exactly the way a run directory's lock is taken, with the same
  open mode and the same lock call, and proves a second handle is kept out of it until it is
  released.

Any failure stops the process with the reason and the flag to set.

| Platform | Accepted | Refused |
|----------|----------|---------|
| Linux | ext2, ext3, ext4, xfs, btrfs, zfs, tmpfs, ramfs, overlay, f2fs, bcachefs, jfs, reiserfs, nilfs2 | NFS, SMB and CIFS, Ceph, AFS, 9p, virtiofs, VirtualBox shared folders, GFS2, OCFS2, Lustre, GPFS, FUSE, and anything not listed |
| macOS | APFS and HFS+ on a local volume | NFS, SMB, AFP, WebDAV, FTP, FUSE, and anything not local |
| FreeBSD | UFS, ZFS, and tmpfs on a local volume | Network filesystems and anything not local |
| Windows | NTFS and ReFS on a local drive | Network drives and shares, FAT, exFAT |

Overlay is accepted because a container's own filesystem is one. From inside the container
SwitchTender cannot see what lies under it, so for overlay the lock probe is what shows its locks
hold.

## How run files are removed

When a run ends, by success, failure, cancel, timeout, or an error in SwitchTender itself, its
directory is removed before the run is recorded as finished.

A process that is killed removes nothing, so every directory carries two things:

- A lock its process holds for the life of the run. The operating system releases it when the
  process dies, however it dies. The lock is the liveness signal.
- A heartbeat counter its process increments every 30 seconds, on a timer rather than on activity.

Every server and worker sweeps its root when it starts and about every five minutes after, the
first periodic sweep within a minute of startup and each later one moved by up to a tenth of the
interval at random, so processes that started together do not sweep together. A lock in the root
lets one process per host sweep a given pass. A sweep deletes a run directory only when all of these
hold:

- Its lock was free at an earlier sweep and is free now.
- The two sweeps are at least five minutes apart on the sweeper's own monotonic clock, which a
  changed wall clock does not move.
- Its heartbeat counter did not change between them.
- Nothing in it changed for at least five minutes.

The counter is never a liveness oracle. It can only hold a deletion back, for the case where a lock
reads free while its process is somehow still counting. A sweeper keeps what it saw in memory, so a
restarted process starts fresh and waits a full five minutes before removing anything, rather than
deleting on one look. In practice a dead run's directory is gone between five and eleven minutes
after any server or worker on the host first sees it.

A process that is stopped, by a debugger, a suspended laptop, or a paused virtual machine, keeps its
lock, and its directory is kept with it.

## A host whose last process died

The sweep runs only while some SwitchTender process runs on the host. For a host whose only worker
stopped for good:

- **systemd.** The shipped units in
  [deploy/systemd](https://github.com/kordloom/switchtender/tree/main/deploy/systemd) put the root
  in the service's runtime directory, which systemd removes when the service stops for any reason, a
  crash included. `PrivateTmp=true` gives the service its own `/tmp`, removed the same way, which
  covers what a tool writes there. systemd sets `RUNTIME_DIRECTORY` from version 240. On an older
  systemd, add
  `--runfiles-dir /run/switchtender/runfiles` to `ExecStart`. `switchtender init --systemd` writes a
  unit with the same run-files settings.
- **tmpfiles.d, as a last resort.** Where the root is the temporary directory and no unit cleans it,
  an age-based rule can. The age must be longer than the longest run allows, which is
  `--run-timeout`, because the rule judges by file times and knows nothing of locks. With a 24 hour
  timeout and the service account's uid 998:

  ```
  # /etc/tmpfiles.d/switchtender.conf
  e /tmp/switchtender-runfiles-998 - - - 26h
  ```

  systemd-tmpfiles-clean runs it about once a day.

## Memory-backed is not RAM-only

The runtime directory, an XDG runtime directory, and a Kubernetes memory volume are tmpfs, which is
memory-backed. Its pages can still be written to swap. On a host that runs SwitchTender:

- Encrypt swap, or turn it off.
- Use full disk encryption, so whatever reaches a disk anyway, through swap, a crash, or a fallback
  root, is unreadable without the key.
- Exclude the run-files root, `/tmp`, and `/var/tmp` from backups.

SwitchTender does not overwrite a file before deleting it. On solid state drives and copy-on-write
filesystems an overwrite lands on new blocks and leaves the old ones as they were, so it would
promise more than it does. Encryption at rest is the control for data that reached a disk.

## Containers

A containerized run's private directory is an in-memory filesystem inside the container, at the same
path it has on the host, so every path the run's environment names resolves the same in both. It is
sized by `--container-runfiles-size`, 64m by default, and mounted nosuid and nodev. It allows exec,
which a container runtime turns off for a tmpfs unless told otherwise, so nothing a tool keeps there
is barred from running. The files staged for the run are bound into it read-only, one by one. What a tool writes
there stays in the container's memory and never reaches the host's disk, the container's writable
layer included. The mount counts against `--container-memory` as it fills.

The host side of the directory is removed by the server or worker as for any run. Nothing waits on
the container dying to clean up.

## Kubernetes

The [Helm chart](https://github.com/kordloom/switchtender/tree/main/deploy/helm) mounts a
memory-backed `emptyDir` at `/run/switchtender` in every server and worker pod and points
`SWITCHTENDER_RUNFILES_DIR` at `/run/switchtender/runfiles`. Its size is `runFiles.sizeLimit`, 256Mi
by default. Without it, run files would sit in the container's writable layer on the node's disk.
For a pod the chart does not manage:

```yaml
containers:
  - name: worker
    env:
      - name: SWITCHTENDER_RUNFILES_DIR
        value: /run/switchtender/runfiles
    volumeMounts:
      - name: runfiles
        mountPath: /run/switchtender
volumes:
  - name: runfiles
    emptyDir:
      medium: Memory
      sizeLimit: 256Mi
```

A memory-backed volume counts against the pod's memory limit. Raise the size for runs that cache
facts for thousands of hosts.

## Tool state

SwitchTender configures known credential-bearing tool state into the run directory. When a run
hands a cloud command line tool a credential, by any credential kind, a federated credential, or a
custom credential type, the variables that tool reads to place its configuration and caches point
inside the run's private directory. What the tool writes about the credential, a token cache or a
configuration filled in by a login command, goes with the run instead of outliving it in the home
directory of the account SwitchTender runs as. Inside a container the same paths fall in the
in-memory mount.

| Tool | Set when the run carries | Variables pointed into the run directory |
|------|--------------------------|------------------------------------------|
| AWS CLI and SDKs | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_ROLE_ARN`, or `AWS_WEB_IDENTITY_TOKEN_FILE` | `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`, `AWS_LOGIN_CACHE_DIRECTORY` |
| Google Cloud CLI | `GOOGLE_APPLICATION_CREDENTIALS`, `GOOGLE_CREDENTIALS`, `GOOGLE_OAUTH_ACCESS_TOKEN`, `CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE`, `CLOUDSDK_AUTH_ACCESS_TOKEN_FILE`, `GCP_SERVICE_ACCOUNT_FILE`, or `GCP_ACCESS_TOKEN` | `CLOUDSDK_CONFIG` |
| Azure CLI | `AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET`, `AZURE_SECRET`, `AZURE_FEDERATED_TOKEN_FILE`, `ARM_CLIENT_ID`, `ARM_CLIENT_SECRET`, or `ARM_OIDC_TOKEN_FILE_PATH` | `AZURE_CONFIG_DIR` |
| kubectl | `KUBECONFIG`, `K8S_AUTH_KUBECONFIG`, or `KUBE_CONFIG_PATH` | `KUBECACHEDIR` |

A variable the run's own credentials already set is left as they set it. A run that hands a tool no
credential keeps that tool's ordinary configuration, so ambient access an operator set up for the
service account keeps working. A run that does hand AWS a credential no longer reads that account's
`~/.aws/config`, so set the region on the credential.

Some tool state has no variable to move it:

- The AWS CLI keeps credentials from a role it assumes in `~/.aws/cli/cache`, and SSO tokens in
  `~/.aws/sso/cache`. To keep the role cache off a host, use the federated AWS credential's exchange
  delivery, which hands the tool ready credentials instead of a role to assume.
- Credential plugins that kubectl runs, such as `aws eks get-token`, `gke-gcloud-auth-plugin`, and
  `kubelogin`, keep their own caches.
- The Ansible event sidecar, which is the run's own output before SwitchTender masks it, stays in the
  temporary directory, where `PrivateTmp` confines it under the shipped units.

## Files rather than environment variables

A secret in an environment variable is inherited by every process the tool starts and can be printed
by any of them. SwitchTender delivers by file wherever the tools a credential kind serves all read
one:

| Credential | Delivery | Why |
|------------|----------|-----|
| SSH key, vault password, become, SSH password, network | File | ansible-playbook reads each from a file or a vars file. |
| Google Cloud service account, kubeconfig | File | Every tool that reads them reads a path. |
| Federated AWS, Google Cloud, and Azure | Token file | The SDKs read the token from a path. The exchange delivery hands over credentials as variables, since that is what it exists for. |
| AWS keys | Variables | A shared credentials file would serve the AWS SDKs, but not a script or tool that reads the variables directly, which is common. |
| Azure service principal | Variables | Terraform's azurerm provider and the Azure SDKs read the client secret from a variable. |
| OpenStack | Variables | openstacksdk can read a `clouds.yaml` file, but scripts and other tools read the `OS_` variables. |
| VMware | Variables | The VMware Ansible collection reads the password from a variable or a module argument. |
| Token, env, and custom variable injectors | Variables | The variable is the contract the operator chose. A custom type can render a file instead. |
| Secret survey answers to scripts and Terraform | Variables | Scripts read `SWITCHTENDER_VAR_<name>` and Terraform reads `TF_VAR_<name>`. Ansible receives them in the extra vars file. |

## Crash reports and core dumps

A crashing process can write its memory to disk, and a server's memory holds decrypted credentials
while a run executes, as does the memory of the tool it handed them to.

- Every server and worker sets its core file size limit to zero at startup, both the soft and the
  hard limit, so it cannot be raised again, and every tool it starts inherits the zero.
- The shipped systemd units set `LimitCORE=0` as well.
- A crash collector the kernel pipes core dumps to, such as systemd-coredump or apport, decides for
  itself what to keep. Check that yours honors the core limit, or turn it off on hosts that run
  SwitchTender.
- On macOS the core limit applies the same way.
- Windows has no core limit. Windows Error Reporting can keep a dump of a crashing process, so check
  its settings on hosts that run SwitchTender, and configure no `LocalDumps` policy that covers
  SwitchTender or the tools it runs.

## Platform notes

- **Linux.** Run under the shipped systemd unit, and run files never reach a disk except through
  swap.
- **macOS.** There is no memory-backed default, so the root is the per-account temporary directory
  under `/var/folders`, private to the account, and the doctor warns. FileVault encrypts it at rest.
- **Windows.** The root is the account's temporary directory, private by access list, and the doctor
  warns. Encrypt the volume with BitLocker.
