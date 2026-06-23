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
#
# IMPORTANT: must be node:22-trixie-slim (Debian Trixie, glibc 2.40), NOT
# node:22-slim (Bookworm, glibc 2.36). The prebuilt @llamaindex/liteparse-
# linux-x64-gnu binary requires GLIBC_2.38/2.39 and GLIBCXX_3.4.31 — these
# are not in Bookworm. Trying alpine fails because liteparse doesn't ship a
# musl variant. Trixie is the right glibc target.
#
# Install pattern: a local project dir so the native binary lands nested
# under @llamaindex/liteparse/node_modules/@llamaindex/liteparse-linux-x64-
# gnu/, which is where the loader resolves it from. Then symlink the bin.
FROM node:22-trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /opt/liteparse \
 && cd /opt/liteparse \
 && npm init -y >/dev/null \
 && npm install --omit=dev --no-audit --no-fund \
      @llamaindex/liteparse @llamaindex/liteparse-linux-x64-gnu \
 && ln -s /opt/liteparse/node_modules/.bin/lit /usr/local/bin/lit \
 && lit --version
COPY --from=builder /build/agent-mem /usr/local/bin/agent-mem
COPY --from=builder /build/migrations /usr/local/share/agent-mem/migrations
WORKDIR /usr/local/share/agent-mem
EXPOSE 34567
CMD ["agent-mem", "worker"]
