FROM golang:1.27 AS build
WORKDIR /src
COPY go.* ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/farstu ./cmd/client
# distroless has no shell; create /data here.
RUN mkdir -p /out/data

# Pinned frpc, checked against the release's frp_sha256_checksums.txt: it
# runs on every user's home server. Upgrade deliberately and test; a new
# version needs new sums.
FROM alpine:3.22 AS frpc
ARG FRP_VERSION=0.71.0
ARG TARGETARCH
RUN case ${TARGETARCH} in \
      amd64) sum=84f27e39f11169f7adcef8e8b70c9329de17747b1f14dad9fb95eef5682ea716 ;; \
      arm64) sum=f33c293c275d8fc68c654b6fba8f10b2551d6463d09a9fc9cffb7227eae82266 ;; \
      arm)   sum=f40a984f83e8d34a9241b0be4a9d5fbcfe513a4a5c022b84a02637ff6d36833b ;; \
      *) echo "no frp checksum for ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
 && wget -qO /tmp/frp.tgz https://github.com/fatedier/frp/releases/download/v${FRP_VERSION}/frp_${FRP_VERSION}_linux_${TARGETARCH}.tar.gz \
 && echo "$sum  /tmp/frp.tgz" | sha256sum -c - \
 && tar xzf /tmp/frp.tgz -C /tmp \
 && mv /tmp/frp_${FRP_VERSION}_linux_${TARGETARCH}/frpc /frpc \
 && mv /tmp/frp_${FRP_VERSION}_linux_${TARGETARCH}/LICENSE /frp-LICENSE

# Root: the client needs the Docker socket, whose group differs per host, and
# access to it is root on the host anyway; nonroot would add setup, not safety.
# frpc, which faces the relay, is started without root (frpc.frpcUID).
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/farstu /farstu
COPY --from=build /out/data /data
COPY --from=frpc /frpc /frpc
# frp's Apache-2.0 license travels with the frpc binary we ship.
COPY --from=frpc /frp-LICENSE /licenses/frp/LICENSE
COPY LICENSE /licenses/farstu/LICENSE
ENTRYPOINT ["/farstu"]
