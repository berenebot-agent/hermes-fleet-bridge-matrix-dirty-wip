#!/bin/sh
# Copy a stopped agent's directory into a fleet template, or warm an existing
# template so its installed tooling travels with every agent created from it.
#
#   ./bake-fleet-template.sh <agent-dir> <template-name>
#       Copy <agent-dir> into FLEET_TEMPLATE_DIR/<template-name> and mark it
#       Clean. The copy is verbatim: files, modes and symlinks as they are.
#
#   ./bake-fleet-template.sh --warm <template-name>
#       Boot an existing template once so its dependency environment installs,
#       then stop it. This is the "spin it up, let the install happen, stop it"
#       step: what lands in <template>/data is what created agents inherit.
#
# Baked templates link duplicate tool binaries to the image and omit uv cache.
# Nothing here provisions Matrix: no account, no room, no bridge involvement.
set -eu
cd "$(dirname "$0")"

FLEET_TEMPLATE_DIR=${FLEET_TEMPLATE_DIR:-$PWD/data/templates}
WAIT=${BAKE_WAIT:-600}

say() { printf '%s\n' "$*" >&2; }
die() { say "bake: $*"; exit 1; }

prune() {
	# Never follow links while pruning an agent-controlled tree.
	[ ! -L "$dir/data" ] || die "data must not be a symlink"
	find "$dir/data" -type d -path '*/installs/*/environments/*/workspace/tools' -prune -exec sh -eu -c '
		for tools do
			for path in "$tools"/python-*-linux-* "$tools"/ffmpeg-*-linux-* \
				"$tools"/chromium-* "$tools"/node-*-linux-* "$tools"/uv-*-linux-* \
				"$tools"/npm-*-linux-* "$tools"/ripgrep-*-linux-*; do
				[ -d "$path" ] && [ ! -L "$path" ] || continue
				target=/opt/hermes/tools/${path##*/}
				rm -rf -- "$path"
				ln -s "$target" "$path"
			done
		done
	' sh {} +
	if [ -d "$dir/data/cache" ] && [ ! -L "$dir/data/cache" ]; then
		rm -rf -- "$dir/data/cache/uv"
	fi
	# shortcut: these tool bundles duplicate the current Hermes image; recheck
	# imports and tool execution when upgrading that image. Keep the venv/lock.
}

warm() {
	name=$1
	case "$name" in
	*[!a-z0-9._-]*|""|.|..) die "invalid template name" ;;
	esac
	dir=$FLEET_TEMPLATE_DIR/$name
	[ -f "$dir/fleet-template.json" ] || die "$dir is not a template"
	[ -d "$dir/data" ] || die "$dir has no data directory"

	say "warming $name: booting once so its tooling installs into $dir/data"
	project="hfb-bake-$$"
	docker compose -p "$project" --project-directory "$dir" -f "$dir/docker-compose.yml" up -d >/dev/null

	waited=0
	while [ "$waited" -lt "$WAIT" ]; do
		if docker compose -p "$project" --project-directory "$dir" -f "$dir/docker-compose.yml" logs 2>&1 |
			grep -Eq '✓ Installing Python dependencies|dependency environment: current'; then
			say "tooling installed after ${waited}s"
			break
		fi
		sleep 5
		waited=$((waited + 5))
	done
	docker compose -p "$project" --project-directory "$dir" -f "$dir/docker-compose.yml" down --timeout 60 >/dev/null 2>&1 || die "shutdown failed; template was not pruned"
	[ "$waited" -lt "$WAIT" ] || die "install did not finish within ${WAIT}s; template was not pruned"
	# A stopped container leaves its live socket behind; it is runtime state.
	find "$dir/data" -type s -delete 2>/dev/null || true
	prune
	say "warmed $name ($(du -sh "$dir/data" | cut -f1) of persistent data)"
}

case "${1:-}" in
--warm)
	[ "$#" -eq 2 ] || die "usage: $0 --warm <template-name>"
	warm "$2"
	;;
"")
	die "usage: $0 <agent-dir> <template-name> | --warm <template-name>"
	;;
*)
	[ "$#" -eq 2 ] || die "usage: $0 <agent-dir> <template-name>"
	src=$1
	name=$2
	dst=$FLEET_TEMPLATE_DIR/$name
	dir=$dst
	[ -d "$src" ] || die "$src is not a directory"
	[ -e "$dst" ] && die "$dst already exists; templates are never overwritten"
	case "$name" in
	*[!a-z0-9._-]*|""|.|..) die "template name must be lowercase letters, digits, dots, dashes or underscores" ;;
	esac

	say "copying $src into $dst"
	mkdir -p "$dst"
	# tar copies files, modes and symlinks verbatim and skips only sockets and
	# devices, which are runtime state rather than agent data.
	tar -C "$src" -cf - . 2>/dev/null | tar -C "$dst" -xf - 2>/dev/null
	# The source's ownership record and template manifest belong to the source.
	rm -f "$dst/.fleet-owner.json" "$dst/fleet-template.json"
	printf '{"Kind":"Clean"}\n' >"$dst/fleet-template.json"
	find "$dst/data" -type s -delete 2>/dev/null || true
	chmod -R go-rwx "$dst" 2>/dev/null || true
	prune
	say "baked $name ($(du -sh "$dst/data" | cut -f1) of persistent data)"
	;;
esac
