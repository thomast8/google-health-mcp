# Remote MCP server image — serves ghealth over Streamable HTTP so it can be added as a custom
# connector in the Claude and ChatGPT apps (incl. mobile). Works on any container host (Railway,
# Fly, Render, Cloud Run, …). The app reads PORT from the environment.
#
# The image ships the whole ghealth CLI, not just the server: the MCP layer runs the CLI as a
# child process so that one implementation backs both surfaces.

FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change reuses the module layer.
COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg

# Static binary: the runtime stage has no libc.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ghealth .

FROM alpine:3.22

# ca-certificates for TLS to googleapis.com; tzdata because rollups are bucketed in the user's
# local days and the CLI needs the zone database to resolve them.
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 ghealth

COPY --from=build /out/ghealth /usr/local/bin/ghealth

USER ghealth
ENV GHEALTH_MCP_HTTP=1 \
    GHEALTH_CONFIG_DIR=/home/ghealth/.config/ghealth \
    HOST=0.0.0.0 \
    PORT=8000

EXPOSE 8000

# GHEALTH_MCP_HTTP=1 selects the Streamable HTTP transport, binding HOST:PORT and serving the MCP
# endpoint at /mcp with a health check at /healthz. The server refuses to start without
# GHEALTH_MCP_TOKEN — it exposes one person's health record, so it will not listen unauthenticated.
CMD ["ghealth", "mcp"]
