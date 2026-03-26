FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o agent-mem ./cmd/agent-mem/

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /build/agent-mem /usr/local/bin/agent-mem
COPY --from=builder /build/migrations /usr/local/share/agent-mem/migrations
WORKDIR /usr/local/share/agent-mem
EXPOSE 34567
CMD ["agent-mem", "worker"]
