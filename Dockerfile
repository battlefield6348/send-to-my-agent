FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /agentapi .

FROM node:24-slim

# codex is a native binary that needs system CA certificates; node-based CLIs bundle their own.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

RUN npm install -g @anthropic-ai/claude-code @openai/codex @google/gemini-cli \
 && npm cache clean --force

# One /skills mount and one /secrets mount serve whichever agent AGENT selects at startup.
RUN mkdir -p /skills /secrets /home/node/workspace \
      /home/node/.claude /home/node/.codex /home/node/.gemini /home/node/.agents \
 && ln -s /skills /home/node/.claude/skills \
 && ln -s /skills /home/node/.agents/skills \
 && ln -s /skills /home/node/.gemini/skills \
 && ln -s /secrets/claude-credentials.json /home/node/.claude/.credentials.json \
 && ln -s /secrets/codex-auth.json /home/node/.codex/auth.json \
 # codex's bubblewrap sandbox cannot create namespaces under Docker's default seccomp, so the container is the sandbox.
 && echo 'sandbox_mode = "danger-full-access"' > /home/node/.codex/config.toml \
 && chown -R node:node /home/node /secrets

COPY --from=build /agentapi /usr/local/bin/agentapi

ENV ADDR=0.0.0.0:8080
USER node
WORKDIR /home/node/workspace
EXPOSE 8080
CMD ["agentapi"]
