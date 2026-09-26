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
    CGO_ENABLED=0 go build -trimpath -o /out/cloudctl ./cmd/cloudctl && \
    CGO_ENABLED=0 go build -trimpath -o /out/devsetup ./cmd/devsetup

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

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 ora && \
    install -d -o ora -g ora -m 0700 /app/logs
WORKDIR /app
COPY --from=build /out/server /out/gateway /out/cloudctl /app/
COPY configs/config.yaml configs/gateway.yaml /app/configs/
COPY --from=web-build /src/frontend/dist /app/web
USER ora
EXPOSE 8080
ENTRYPOINT ["/app/server"]
