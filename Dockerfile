FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o agent-mem ./cmd/agent-mem/

# node:22-slim (Debian Bookworm) ships node + npm preinstalled. We use them
# only to install the LiteParse CLI; the binary itself is a native artifact
# under @llamaindex/liteparse.
#
# IMPORTANT: must be a glibc image, NOT alpine. LiteParse's npm package
# only ships @llamaindex/liteparse-linux-x64-gnu and -linux-arm64-gnu as
# optional native deps — no musl variant. On alpine the native module fails
# to load with "Failed to load native module for linux-x64".
#
# LiteParse handles PDF/DOCX/XLSX/PPTX text + screenshots locally before
# describe_attachment falls back to Gemini multimodal. The build fails fast
# via `lit --version` if install breaks. LibreOffice (needed by LiteParse for
# DOCX/XLSX/PPTX conversion) is NOT installed yet — only PDF + image paths
# are exercised in v1. Add `libreoffice` + `imagemagick` here when office-doc
# parsing is needed.
#
# The Go binary copied from the alpine builder is statically linked
# (CGO_ENABLED=0) so it runs on Debian without modification.
FROM node:22-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g @llamaindex/liteparse \
 && lit --version
COPY --from=builder /build/agent-mem /usr/local/bin/agent-mem
COPY --from=builder /build/migrations /usr/local/share/agent-mem/migrations
WORKDIR /usr/local/share/agent-mem
EXPOSE 34567
CMD ["agent-mem", "worker"]
