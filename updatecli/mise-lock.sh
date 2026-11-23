#!/bin/sh
# Updates mise.lock after an Updatecli target changed mise.toml. Updatecli also
# runs the command of a shell target when it only shows a diff (DRY_RUN=true):
# the lockfile must then stay as it is.
set -eu
if [ "${DRY_RUN:-false}" = "true" ]; then
	exit 0
fi
mise trust --yes mise.toml
mise lock
