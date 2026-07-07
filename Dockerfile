# Builds both binaries: the archi CLI and the archi-app GitHub App service.
# The runtime image needs git (the app clones per-job workspaces) and CA
# certificates (provider + GitHub API calls).
#
#   docker build -t archi-app .
#   docker run -e APP_ID=… -e WEBHOOK_SECRET=… -e PRIVATE_KEY_BASE64=… \
#     -v archi-cache:/var/lib/archi-app -p 8443:8443 archi-app

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/archi .
RUN cd app && CGO_ENABLED=0 go build -o /out/archi-app .

FROM alpine:3.20
RUN apk add --no-cache git ca-certificates
COPY --from=build /out/archi /out/archi-app /usr/local/bin/
VOLUME /var/lib/archi-app
ENV CACHE_DIR=/var/lib/archi-app
EXPOSE 8443
ENTRYPOINT ["archi-app", "serve", "-addr", ":8443"]
