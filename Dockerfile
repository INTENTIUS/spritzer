# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.25 AS build
WORKDIR /src

# Cache module downloads first (there are none today, but this keeps the layer
# stable if dependencies are ever added).
COPY go.mod ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO disabled for a fully static binary that runs on distroless.
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/spritzer ./cmd/spritzer

# ---- final stage ----
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/spritzer /usr/local/bin/spritzer
EXPOSE 4290
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/spritzer"]
