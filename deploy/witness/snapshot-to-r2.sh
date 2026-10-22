#!/usr/bin/env bash
# snapshot-to-r2.sh copies the hosted witness's state directory to R2 as one compressed tar, then
# prunes older snapshots so the bucket stays well under R2's free 10 GB. switchtender-witness-
# snapshot.timer runs it every hour as root. README.md beside it is the runbook.
#
# The service keeps running while the tar is taken. Its checkpoint and enrollment files are replaced
# by rename, so the tar holds one whole version of each. Its head logs and finding records are
# appended a line at a time, so the tar can end one of them partway through a line. restore.sh cuts
# that half line off before the service reads it again.
#
# The witness signing key is never in a snapshot. It lives in its own key directory, and this script
# refuses to run when a key file sits in the state directory, because that means the service was
# started without --key-dir and the next upload would put the key in the bucket.
#
# Settings, all optional:
#   RCLONE_CONFIG          the rclone config holding the R2 credentials
#                          (default /etc/switchtender-witness/rclone.conf)
#   SNAPSHOT_STATE_DIR     the directory to copy (default /var/lib/switchtender-witness)
#   SNAPSHOT_REMOTE        the rclone path snapshots go to
#                          (default r2:switchtender-witness-state/snapshots)
#   SNAPSHOT_KEEP          the most snapshots kept (default 48, two days of hourly runs)
#   SNAPSHOT_BUDGET_BYTES  the most bytes kept (default 5368709120, which is 5 GiB)
#
# The newest snapshot is always kept. After it, snapshots are kept newest first until either limit
# is reached, and every older one is deleted. Nothing is deleted unless an upload just succeeded, so
# a droplet that stops writing snapshots never empties the bucket.
set -euo pipefail

export RCLONE_CONFIG="${RCLONE_CONFIG:-/etc/switchtender-witness/rclone.conf}"
state_dir="${SNAPSHOT_STATE_DIR:-/var/lib/switchtender-witness}"
remote="${SNAPSHOT_REMOTE:-r2:switchtender-witness-state/snapshots}"
keep="${SNAPSHOT_KEEP:-48}"
budget="${SNAPSHOT_BUDGET_BYTES:-5368709120}"
key_file="witness-key.json"
name_form='^witness-state-[0-9]{8}T[0-9]{6}Z\.tar\.gz$'
work=""
trap 'if [[ -n "$work" ]]; then rm -rf "$work"; fi' EXIT

# say prints one progress line for the journal.
say() { printf 'snapshot: %s\n' "$*"; }

# die prints why the snapshot stopped and exits nonzero, so the unit shows as failed.
die() {
	printf 'snapshot: %s\n' "$*" >&2
	exit 1
}

# prune keeps the snapshot just uploaded and the newest others within both limits, and deletes the
# rest. The listing is sorted by name, which sorts by time because every name is a UTC timestamp.
prune() {
	local uploaded="$1" kept=0 total=0 deleted=0 by_budget=0 listing size file
	listing="$(rclone lsf --files-only --format sp --separator ' ' "$remote/")"
	while read -r size file; do
		[[ "$file" =~ $name_form ]] || continue
		if [[ "$file" == "$uploaded" ]] || { ((deleted == 0)) && ((kept < keep)) &&
			((total + size <= budget)); }; then
			kept=$((kept + 1))
			total=$((total + size))
			continue
		fi
		if ((deleted == 0)) && ((kept < keep)); then
			by_budget=1
		fi
		rclone deletefile "$remote/$file"
		deleted=$((deleted + 1))
	done < <(sort -k2,2r <<<"$listing")
	say "kept $kept snapshots, $total bytes, deleted $deleted"
	if ((by_budget)); then
		say "warning: the byte budget, not the count, set how many snapshots are kept. Only $kept" \
			"fit in $budget bytes. Raise SNAPSHOT_BUDGET_BYTES while staying well under 10 GB, or" \
			"keep fewer by lowering SNAPSHOT_KEEP."
	fi
}

# main takes the snapshot, uploads it, checks it arrived whole, and prunes.
main() {
	[[ "$keep" =~ ^[1-9][0-9]*$ ]] || die "SNAPSHOT_KEEP must be a whole number of at least 1"
	[[ "$budget" =~ ^[1-9][0-9]*$ ]] || die "SNAPSHOT_BUDGET_BYTES must be a whole number of bytes"
	[[ -d "$state_dir" ]] || die "no state directory at $state_dir"
	if [[ -e "$state_dir/$key_file" ]]; then
		die "$state_dir/$key_file exists, so the service was started without --key-dir. Refusing" \
			"to upload the witness signing key. Move it to the key directory the unit names and" \
			"restart the service."
	fi

	local name status=0
	name="witness-state-$(date -u +%Y%m%dT%H%M%SZ).tar.gz"
	work="$(mktemp -d)"

	# GNU tar exits 1 when a file changed while it was read, which an append during the run causes.
	# The archive is still whole, so 1 is accepted and anything higher is not.
	tar --create --gzip --file "$work/$name" --directory "$state_dir" \
		--exclude="$key_file" --exclude='*.tmp-*' --warning=no-file-changed . || status=$?
	((status <= 1)) || die "tar failed with status $status"
	tar --list --gzip --file "$work/$name" >/dev/null || die "the new archive does not read back"
	if tar --list --gzip --file "$work/$name" | grep -F "$key_file" >/dev/null; then
		die "the new archive holds $key_file. Refusing to upload it."
	fi

	rclone copyto "$work/$name" "$remote/$name"
	rclone check --one-way "$work" "$remote" --include "$name" >/dev/null 2>&1 ||
		die "$name did not arrive whole at $remote"
	say "uploaded $name, $(stat -c %s "$work/$name") bytes"
	prune "$name"
}

main "$@"
