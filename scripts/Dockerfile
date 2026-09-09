FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/server ./cmd/server && \
    CGO_ENABLED=0 go build -trimpath -o /out/cloudctl ./cmd/cloudctl

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 ora
WORKDIR /app
COPY --from=build /out/server /out/cloudctl /app/
COPY configs/config.yaml /app/configs/config.yaml
USER ora
EXPOSE 8080
ENTRYPOINT ["/app/server"]
