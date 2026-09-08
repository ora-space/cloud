# Build stage
FROM golang:alpine AS builder

WORKDIR /app

# Enable Go modules and set GOPROXY
ENV GO111MODULE=on \
    GOPROXY=https://goproxy.cn,direct

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o server cmd/server/main.go

# Run stage
FROM alpine:latest

WORKDIR /app

# Install basic dependencies (certificates, timezone)
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Asia/Shanghai

COPY --from=builder /app/server /app/server
COPY --from=builder /app/configs /app/configs

EXPOSE 8080

ENTRYPOINT ["/app/server"]
