#!/usr/bin/env bash
# restore.sh puts the hosted witness's state directory back from a snapshot that snapshot-to-r2.sh
# uploaded, and leaves the memory-restored marker the service turns into one memory_restored note
# per enrolled install on its next start. README.md beside it is the runbook. Run it as root with
# the service stopped:
#
#   restore.sh                                          restore the newest snapshot
#   restore.sh witness-state-20261021T150000Z.tar.gz    restore that snapshot
#
# A restored memory is older than what the installs last had countersigned. Their next pushes name a
# size the witness no longer holds, the witness answers with the size it does hold, and each install
# proves again from there. The note records that the witness's own memory rewound and to where.
#
# The state directory it replaces is moved aside, never deleted. A head log or finding record that
# the snapshot caught partway through a line is cut back to its last whole line, because the next
# line the service appended would otherwise join the fragment and be skipped by every later reader.
#
# Settings, all optional:
#   RCLONE_CONFIG      the rclone config holding the R2 credentials
#                      (default /etc/switchtender-witness/rclone.conf)
#   RESTORE_STATE_DIR  the state directory to restore (default /var/lib/switchtender-witness)
#   RESTORE_KEY_DIR    the key directory the unit names (default /var/lib/switchtender-witness-key)
#   RESTORE_REMOTE     the rclone path snapshots live at
#                      (default r2:switchtender-witness-state/snapshots)
#   RESTORE_UNIT       the service that must be stopped (default switchtender-witness)
#   RESTORE_OWNER      the account that owns the state (default switchtender-witness)
set -euo pipefail

export RCLONE_CONFIG="${RCLONE_CONFIG:-/etc/switchtender-witness/rclone.conf}"
state_dir="${RESTORE_STATE_DIR:-/var/lib/switchtender-witness}"
key_dir="${RESTORE_KEY_DIR:-/var/lib/switchtender-witness-key}"
remote="${RESTORE_REMOTE:-r2:switchtender-witness-state/snapshots}"
unit="${RESTORE_UNIT:-switchtender-witness}"
owner="${RESTORE_OWNER:-switchtender-witness}"
key_file="witness-key.json"
marker="memory-restored"
name_form='^witness-state-[0-9]{8}T[0-9]{6}Z\.tar\.gz$'
work=""
trap 'if [[ -n "$work" ]]; then rm -rf "$work"; fi' EXIT

# say prints one progress line.
say() { printf 'restore: %s\n' "$*"; }

# die prints why the restore stopped and exits nonzero, having changed nothing it has not reported.
die() {
	printf 'restore: %s\n' "$*" >&2
	exit 1
}

# trim_torn_tail cuts a file back to its last newline when it ends partway through a line.
trim_torn_tail() {
	local file="$1" size last torn
	size="$(stat -c %s "$file")"
	((size > 0)) || return 0
	last="$(tail -c 1 "$file" | od -An -tx1 | tr -d ' \n')"
	[[ "$last" == "0a" ]] && return 0
	torn="$(tail -n 1 "$file" | wc -c)"
	truncate -s "$((size - torn))" "$file"
	say "cut a half-written last line of $torn bytes from ${file#"$work/state/"}"
}

# newest names the most recent snapshot at the remote, or nothing when there is none.
newest() {
	local listing
	listing="$(rclone lsf --files-only "$remote/")"
	grep -E "$name_form" <<<"$listing" | sort | tail -n 1 || true
}

# main checks every precondition before touching the state directory, then swaps the restored copy
# in with the marker already inside it.
main() {
	(($# <= 1)) || die "usage: restore.sh [snapshot name]"
	((EUID == 0)) || die "run as root, so the restored files can be given to $owner"
	id -u "$owner" >/dev/null 2>&1 || die "there is no $owner account. Create it as the runbook's" \
		"install step does, then run this again."
	if systemctl is-active --quiet "$unit"; then
		die "$unit is running. Stop it first with: systemctl stop $unit"
	fi
	if [[ ! -f "$key_dir/$key_file" ]]; then
		die "there is no witness key at $key_dir/$key_file. Restore it from the offline copy first," \
			"as the runbook says. Starting the service without it would create a new key, and every" \
			"relying party pinned to the published key id would stop trusting this witness."
	fi

	local name="${1:-}"
	if [[ -z "$name" ]]; then
		name="$(newest)"
		[[ -n "$name" ]] || die "no snapshot found at $remote"
	fi
	[[ "$name" =~ $name_form ]] || die "$name is not a snapshot name"

	work="$(mktemp -d)"
	rclone copyto "$remote/$name" "$work/$name"
	tar --list --gzip --file "$work/$name" >/dev/null || die "$name does not read back"
	if tar --list --gzip --file "$work/$name" | grep -F "$key_file" >/dev/null; then
		die "$name holds $key_file, which no snapshot may. Not restoring it."
	fi
	mkdir -m 0700 "$work/state"
	tar --extract --gzip --file "$work/$name" --directory "$work/state" --no-same-owner

	local file
	while IFS= read -r -d '' file; do
		trim_torn_tail "$file"
	done < <(find "$work/state" -type f -name '*.jsonl' -print0)
	printf 'restored from %s at %s\n' "$name" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
		>"$work/state/$marker"
	chown -R "$owner:$owner" "$work/state"
	chmod 0700 "$work/state"
	chmod 0600 "$work/state/$marker"

	# The copy is staged beside the state directory, on the same file system, so the swap below is
	# two renames and the service never sees a directory that is half written.
	local staged="$state_dir.restoring" aside=""
	rm -rf "$staged"
	mv "$work/state" "$staged"
	if [[ -e "$state_dir" ]]; then
		aside="$state_dir.before-restore-$(date -u +%Y%m%dT%H%M%SZ)"
		mv "$state_dir" "$aside"
	fi
	mv "$staged" "$state_dir"

	say "restored $name into $state_dir"
	if [[ -n "$aside" ]]; then
		say "the state it replaced is kept at $aside"
	fi
	say "start the service with: systemctl start $unit"
	say "its log then shows one memory_restored note per install, and $marker is gone from" \
		"$state_dir once every note is recorded"
}

main "$@"
