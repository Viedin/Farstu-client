FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.* ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-X main.version=${VERSION}" -o /out/farstu ./cmd/client
# distroless has no shell; create /data here.
RUN mkdir -p /out/data

# Stock frp source, rebuilt because upstream binaries bundle an old Go runtime.
# Keep this version, checksum and dependency patches in step with the relay.
FROM golang:1.27.3 AS frpc
WORKDIR /src
RUN go mod download github.com/fatedier/frp@v0.71.0 \
 && test "$(cat /go/pkg/mod/cache/download/github.com/fatedier/frp/@v/v0.71.0.ziphash)" = "h1:hrzMepFp/asl2oAxxLMgffCI/wrzvg6lZyMzxEEnWbQ=" \
 && cp -R /go/pkg/mod/github.com/fatedier/frp@v0.71.0/. . \
 && chmod -R u+w . \
 && go get golang.org/x/net@v0.62.0 github.com/Azure/go-ntlmssp@v0.1.1 \
 && CGO_ENABLED=0 go build -trimpath -tags=frpc,noweb -o /frpc ./cmd/frpc

# Root: the client needs the Docker socket, whose group differs per host, and
# access to it is root on the host anyway; nonroot would add setup, not safety.
# frpc, which faces the relay, is started without root (frpc.frpcUID).
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/farstu /farstu
COPY --from=build /out/data /data
COPY --from=frpc /frpc /frpc
# frp's Apache-2.0 license travels with the frpc binary we ship.
COPY --from=frpc /src/LICENSE /licenses/frp/LICENSE
COPY LICENSE /licenses/farstu/LICENSE
ENTRYPOINT ["/farstu"]
