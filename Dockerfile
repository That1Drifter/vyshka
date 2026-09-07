# The reference hub as a container: a static, cgo-free binary on a distroless
# base, running as a non-root user. The image is the single-binary install
# story with a filesystem around it, nothing more.
#
#   docker build -t vyshka-hub .
#   docker run --rm -p 127.0.0.1:8080:8080 -v vyshka-data:/data vyshka-hub
#
# The admin token is best passed as a file (VYSHKA_ADMIN_TOKEN=file:/run/secrets/admin_token)
# so it never appears in the process environment; deploy/docker-compose.yml shows the shape.

FROM golang:1.26.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X github.com/That1Drifter/vyshka/hub.Version=${VERSION}" \
    -o /out/vyshka-hub ./hub/cmd/vyshka-hub

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/vyshka-hub /vyshka-hub
# The SQLite file lives on a volume; the non-root user (65532) must own it on
# the host side when the volume is a bind mount.
VOLUME /data
ENV VYSHKA_ADDR=0.0.0.0:8080 \
    DATABASE_URL=/data/vyshka.db
EXPOSE 8080
ENTRYPOINT ["/vyshka-hub"]
CMD ["serve"]
