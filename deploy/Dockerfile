# syntax=docker/dockerfile:1
# Static, CGO-free binary on a distroless base: no shell, no package manager,
# runs as an unprivileged user.

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mevlens ./cmd/mevlens

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/mevlens /usr/local/bin/mevlens
USER nonroot:nonroot
EXPOSE 9464
ENTRYPOINT ["/usr/local/bin/mevlens"]
