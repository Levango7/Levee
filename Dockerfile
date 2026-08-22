# syntax=docker/dockerfile:1
#
# Multi-stage Dockerfile for levee.
#
# Stage 1 (node):       installs npm dependencies and builds the Vue frontend.
# Stage 2 (builder):    Go 1.25 image compiles the CLI binary with version
#                       injection and static linking (CGO_ENABLED=0).
# Stage 3 (dist):       copies the binary plus the embedded frontend assets
#                       from internal/web/dist so the runtime image is minimal.
# Stage 4 (runtime):    Alpine-based image with only the binary and CA certs.
#
# Build:
#   docker build -t levee:dev .
#
# Run:
#   docker run --rm -p 8080:8080 -p 9090:9090 levee:dev serve

ARG GO_VERSION=1.25
ARG ALPINE_VERSION=3.20
ARG NODE_VERSION=20-alpine

# ---------------------------------------------------------------------------
# Stage 1: node — build frontend assets
# ---------------------------------------------------------------------------
FROM node:${NODE_VERSION} AS node

WORKDIR /src/web

COPY web/package.json web/package-lock.json ./
RUN npm ci --prefer-offline

COPY web/ .
RUN npm run build

# ---------------------------------------------------------------------------
# Stage 2: builder
# ---------------------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Cache Go module downloads.
COPY go.mod go.sum ./
RUN go mod download

# Copy source.
COPY . .

# Copy built frontend assets from node stage.
COPY --from=node /src/web/dist ./internal/web/dist

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w \
          -X main.version=${VERSION} \
          -X main.commitHash=${COMMIT} \
          -X main.buildTime=${BUILD_TIME}" \
        -o /out/levee ./cmd/levee

# ---------------------------------------------------------------------------
# Stage 3: dist (asset packaging)
# ---------------------------------------------------------------------------
FROM scratch AS dist

COPY --from=builder /out/levee /levee
COPY --from=builder /src/internal/web/dist /dist
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /ca-certificates.crt

# ---------------------------------------------------------------------------
# Stage 4: runtime
# ---------------------------------------------------------------------------
FROM alpine:${ALPINE_VERSION}

RUN apk add --no-cache tzdata dumb-init && \
    cp /usr/share/zoneinfo/UTC /etc/localtime && \
    echo "UTC" > /etc/timezone

COPY --from=dist /levee /usr/local/bin/levee
COPY --from=dist /dist /var/lib/levee/dist
COPY --from=dist /ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

RUN addgroup -Slevee && adduser -Slevee -Glevee && \
    chown -R levee:levee /var/lib/levee

USER levee
WORKDIR /home/levee

EXPOSE 8080 9090

ENTRYPOINT ["dumb-init", "--"]
CMD ["levee", "serve", "--addr", ":9090"]
