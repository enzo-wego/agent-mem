FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o agent-mem ./cmd/agent-mem/

FROM alpine:3.19
# - ca-certificates: HTTPS to Slack/Jira/GH/CF/PD/DD/Sentry/GWS
# - nodejs + npm: needed only to install the LiteParse CLI; the binary itself
#   ships as a native artifact under @llamaindex/liteparse.
# - LiteParse handles PDF/DOCX/XLSX/PPTX text + screenshots locally before
#   describe_attachment falls back to Gemini multimodal. If install fails the
#   build fails fast via `lit --version`.
# - LibreOffice (needed by LiteParse for DOCX/XLSX/PPTX conversion) is NOT
#   installed yet — only PDF + image paths are exercised in v1. Add
#   `libreoffice` + `imagemagick` here when office-doc parsing is needed.
RUN apk add --no-cache ca-certificates nodejs npm \
 && npm install -g @llamaindex/liteparse \
 && lit --version
COPY --from=builder /build/agent-mem /usr/local/bin/agent-mem
COPY --from=builder /build/migrations /usr/local/share/agent-mem/migrations
WORKDIR /usr/local/share/agent-mem
EXPOSE 34567
CMD ["agent-mem", "worker"]
