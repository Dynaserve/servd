# Build a fully static binary — pure Go (pgx included), so no C deps.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/platform ./cmd/servd
# Pre-create the data dir owned by the distroless nonroot uid (65532) so a
# freshly-created volume is writable by the unprivileged runtime user.
RUN mkdir -p /data && chown -R 65532:65532 /data

# Minimal runtime: distroless static (no shell, no libc needed for a static Go binary).
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/platform /app/platform
COPY --from=build --chown=65532:65532 /data /data

# Persist the store outside the image. Mount a volume at /data.
ENV DATA_FILE=/data/store.json
ENV LISTEN_ADDR=:8080
VOLUME ["/data"]
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/app/platform"]
