# ---- build stage: compile a static binary ----
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /gopherit ./cmd/api

# ---- run stage: tiny image, just the binary ----
FROM gcr.io/distroless/static-debian12
COPY --from=build /gopherit /gopherit
EXPOSE 4000
VOLUME /data
ENTRYPOINT ["/gopherit", "-dsn", "/data/gopherit.db", "-env", "production"]
