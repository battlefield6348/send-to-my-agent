FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /agentapi .

FROM node:24-slim
ARG AGENT=claude

# codex is a native binary that needs system CA certificates; node-based CLIs bundle their own.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# Each CLI reads user skills from its own folder; all of them point at /skills so one mount works for any agent.
RUN case "$AGENT" in \
      claude) cli_package=@anthropic-ai/claude-code; config_dir=/home/node/.claude; skills_dir=/home/node/.claude/skills ;; \
      codex)  cli_package=@openai/codex;             config_dir=/home/node/.codex;  skills_dir=/home/node/.agents/skills ;; \
      gemini) cli_package=@google/gemini-cli;        config_dir=/home/node/.gemini; skills_dir=/home/node/.gemini/skills ;; \
      *) echo "unknown AGENT: $AGENT (use claude, codex or gemini)" >&2; exit 1 ;; \
    esac \
 && npm install -g "$cli_package" \
 && npm cache clean --force \
 # Pre-create the config dir so credential file mounts do not leave it owned by root.
 && mkdir -p /skills /home/node/workspace "$config_dir" "$(dirname "$skills_dir")" \
 && ln -s /skills "$skills_dir" \
 # codex's bubblewrap sandbox cannot create namespaces under Docker's default seccomp, so the container is the sandbox.
 && if [ "$AGENT" = codex ]; then echo 'sandbox_mode = "danger-full-access"' > "$config_dir/config.toml"; fi \
 && chown -R node:node /home/node

COPY --from=build /agentapi /usr/local/bin/agentapi

ENV AGENT=$AGENT \
    ADDR=0.0.0.0:8080
USER node
WORKDIR /home/node/workspace
EXPOSE 8080
CMD ["agentapi"]
