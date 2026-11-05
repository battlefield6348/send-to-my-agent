#!/bin/sh
set -eu

# Copy credentials instead of linking them, so they are readable by node whoever owns them on the host,
# and token refreshes inside the container never write back to the mounted files.
copy_credential() {
	if [ -f "/secrets/$1" ]; then
		install -o node -g node -m 600 "/secrets/$1" "$2"
	fi
}
copy_credential .credentials.json /home/node/.claude/.credentials.json
copy_credential auth.json /home/node/.codex/auth.json

exec setpriv --reuid=node --regid=node --init-groups "$@"
