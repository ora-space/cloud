# Ora Cloud images. The default (last) target is the service image: server, gateway, cloudctl and
# the built frontend, which the Gateway serves when GATEWAY_WEB_DIST_DIR=/app/web. The devsetup
# target serves local stacks such as the ora-space/cluster Compose file:
#
#   docker build -f scripts/Dockerfile -t ora-cloud:phase-one .
#   docker build -f scripts/Dockerfile --target devsetup -t ora-cloud-devsetup:local .
FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/server ./cmd/server && \
    CGO_ENABLED=0 go build -trimpath -o /out/gateway ./cmd/gateway && \
    CGO_ENABLED=0 go build -trimpath -o /out/model-gateway ./cmd/model-gateway && \
    CGO_ENABLED=0 go build -trimpath -o /out/cloudctl ./cmd/cloudctl && \
    CGO_ENABLED=0 go build -trimpath -o /out/devsetup ./cmd/devsetup && \
    CGO_ENABLED=0 go build -trimpath -o /out/credential-store ./cmd/credential-store && \
    CGO_ENABLED=0 go build -trimpath -o /out/clone-executor ./cmd/clone-executor && \
    CGO_ENABLED=0 go build -trimpath -o /out/git-credential-ora ./cmd/git-credential-ora

FROM node:24-alpine AS web-build
WORKDIR /src/frontend
# engine-strict (frontend/.npmrc) requires the npm version the lock file was written with.
RUN npm install -g npm@11
COPY frontend/package.json frontend/package-lock.json frontend/.npmrc ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# Local-development key and migration setup (cmd/devsetup): generates the Gateway/Cloud key pairs
# into .local/gateway and applies migrations. Never part of the service image.
FROM alpine:3.22 AS devsetup
RUN adduser -D -u 10001 ora && install -d -o ora -g ora -m 0700 /app/.local /app/.local/gateway
WORKDIR /app
COPY --from=build /out/devsetup /app/
USER ora
ENTRYPOINT ["/app/devsetup"]

# The credentialed-clone executor: the credential-executing identity C of the approved Node isolation
# decision, as a deployment unit of its own (20261012-credential-executor-belongs-to-the-current-product
# D1). It is a separate target for two reasons. It is the only image that needs a real `git`, and the
# Cloud service image should not carry one. And it runs as its own uid — 10002, distinct from the
# service's 10001 and from the Agent workload's 1000 in the sandbox Node image — so that the boundary
# around the materialization root is enforced by the kernel and not only by which volume a container
# was given.
#
# It is declared *before* the service stage on purpose: Docker builds the last stage when no `--target`
# is given, and the service image is the documented default. Appending this stage after it would have
# silently made every untargeted `docker build -t ora-cloud:local` produce the executor instead, and
# the `cloud`, `gateway` and `credential-store` services — all of which run `ora-cloud:local` — would
# have failed to start with a missing binary rather than with a configuration error.
FROM alpine:3.22 AS executor
RUN apk add --no-cache ca-certificates tzdata git && adduser -D -u 10002 executor
WORKDIR /app
# Both binaries stay root-owned and are never writable by C. The helper in particular is on the path
# Git executes: a writable helper would be a way to make the executor hand the token to something else
# (isolation D2, D11).
COPY --from=build /out/clone-executor /out/git-credential-ora /app/
USER executor
EXPOSE 8444
ENTRYPOINT ["/app/clone-executor"]

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 ora && \
    install -d -o ora -g ora -m 0700 /app/logs
WORKDIR /app
COPY --from=build /out/server /out/gateway /out/model-gateway /out/cloudctl /out/credential-store /app/
COPY configs/config.yaml configs/gateway.yaml /app/configs/
COPY --from=web-build /src/frontend/dist /app/web
USER ora
EXPOSE 8080
ENTRYPOINT ["/app/server"]
