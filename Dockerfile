# Builds cmd/server -- the real HTTP entry point (see docs/running-locally.md).
# main.go at the repo root is a separate router-only demo and is not built here.
FROM golang:1.25-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/server ./cmd/server

FROM alpine:3.20

RUN addgroup -S neochat && adduser -S -G neochat neochat

WORKDIR /app
COPY --from=build /out/server ./server
# Config/prompt files are read from disk at startup by relative path
# (configs/*.json, prompts/*.md) -- see cmd/server/main.go.
COPY configs ./configs
COPY prompts ./prompts

USER neochat
EXPOSE 8080

HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=10 \
    CMD wget -q -O- http://127.0.0.1:8080/health || exit 1

ENTRYPOINT ["./server"]
