# Multi-stage build producing a static binary on scratch.
#
# The result is a couple of megabytes with no shell, no package manager, and no
# CVE surface from a base distro — which matters more than usual here, because
# this image is meant to sit in a namespace running continuously for months.

FROM golang:1.26-alpine AS build

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

WORKDIR /src

# Copy manifests first so dependency download is cached across source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off and -trimpath give a reproducible, fully static binary.
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w \
      -X github.com/ashdaily/spoofy/internal/version.Version=${VERSION} \
      -X github.com/ashdaily/spoofy/internal/version.Commit=${COMMIT} \
      -X github.com/ashdaily/spoofy/internal/version.Date=${DATE}" \
    -o /out/spoofy ./cmd/spoofy


FROM scratch

# CA certificates, so an https:// target works.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/spoofy /spoofy

# Non-root by default. Nothing here needs privileges, and a traffic generator
# is a poor place to hand any out.
USER 65534:65534

EXPOSE 9090

ENTRYPOINT ["/spoofy"]
CMD ["run"]
