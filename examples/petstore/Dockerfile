# The demo target API, built the same way as Spoofy so the compose stack needs
# nothing from the internet beyond Prometheus and Grafana.

FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/petstore ./examples/petstore


FROM scratch
COPY --from=build /out/petstore /petstore
USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["/petstore"]
