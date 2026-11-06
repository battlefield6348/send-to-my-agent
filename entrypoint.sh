#!/bin/sh
set -eu

# /home/node/.claude may be a host folder mounted to keep claude's own login across rebuilds,
# so its ownership, skills link and settings are set here instead of in the image.
claude_dir=/home/node/.claude
chown -R node:node "$claude_dir"
if [ -e "$claude_dir/skills" ] && [ ! -L "$claude_dir/skills" ]; then
	echo "warning: $claude_dir/skills is a real folder, so /skills is not linked there" >&2
else
	ln -sfn /skills "$claude_dir/skills"
fi
# claude refuses unapproved commands in -p mode, so pre-approve node scripts shipped in the mounted skills.
echo '{"permissions":{"allow":["Bash(node /skills/*)"]}}' > "$claude_dir/settings.json"
chown -h node:node "$claude_dir/skills" "$claude_dir/settings.json"

# Copy credentials instead of linking them, so they are readable by node whoever owns them on the host,
# and token refreshes inside the container never write back to the mounted files.
# Existing credentials are kept so a login refreshed inside the container is not overwritten by an older copy.
copy_credential() {
	if [ -f "/secrets/$1" ] && [ ! -f "$2" ]; then
		install -o node -g node -m 600 "/secrets/$1" "$2"
	fi
}
copy_credential .credentials.json "$claude_dir/.credentials.json"
copy_credential auth.json /home/node/.codex/auth.json

exec setpriv --reuid=node --regid=node --init-groups "$@"
