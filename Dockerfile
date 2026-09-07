# The reference hub as a container: a static, cgo-free binary on a distroless
# base, running as a non-root user. The image is the single-binary install
# story with a filesystem around it, nothing more.
#
#   docker build -t vyshka-hub .
#   docker run --rm -p 127.0.0.1:8080:8080 -v vyshka-data:/data vyshka-hub
#
# The admin token is best passed as a file (VYSHKA_ADMIN_TOKEN=file:/run/secrets/admin_token)
# so it never appears in the process environment; deploy/docker-compose.yml shows the shape.
#
# Both base images are pinned by digest so that rebuilding a commit rebuilds
# the same image; bump the tag and the digest together
# (docker buildx imagetools inspect <ref> --format '{{.Manifest.Digest}}').

FROM golang:1.26.4-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X github.com/That1Drifter/vyshka/hub.Version=${VERSION}" \
    -o /out/vyshka-hub ./hub/cmd/vyshka-hub
# The data directory is created here, owned by the runtime user, and copied
# into the final image before VOLUME: a fresh named or anonymous volume
# inherits it, so the non-root hub can create its database on first boot.
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/vyshka-hub /vyshka-hub
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME /data
ENV VYSHKA_ADDR=0.0.0.0:8080 \
    DATABASE_URL=/data/vyshka.db
EXPOSE 8080
ENTRYPOINT ["/vyshka-hub"]
CMD ["serve"]
