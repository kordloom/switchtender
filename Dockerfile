# Build the single static binary, then run it on a small image that carries ansible.
# The build stage runs on the build platform and cross-compiles, so multi-arch images
# never compile Go under emulation.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
# The official image pins GOTOOLCHAIN=local, and go.mod requires a patch release newer than the one
# any published golang image carries, because it was raised to pick up a standard library fix. With
# the pin left alone every container build failed outright with "go.mod requires go >= 1.26.6", so
# the image in the release workflow could not be produced at all. Letting the toolchain resolve
# fetches exactly the version go.mod names.
ENV GOTOOLCHAIN=auto
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/kordloom/switchtender/cmd.Version=$VERSION" -o /switchtender .

# 3.20 left support on 2026-04-01, so the image stopped receiving security patches while it was
# still being built and published. Track a supported branch and move it on before the next lapses.
FROM alpine:3.22
RUN apk add --no-cache ansible-core openssh-client ca-certificates

# The control plane executes other people's playbooks, so it does not run them as root. The image ran
# as root because nothing said otherwise, which meant a container escape, a mounted socket, or a tool
# that writes outside its workdir all started with the highest privilege the container had. /data is
# the working directory so the default database path lands somewhere the user owns; mount a volume
# there and give it the same uid.
RUN addgroup -g 10001 -S switchtender \
    && adduser -u 10001 -S -G switchtender -h /data switchtender \
    && mkdir -p /data \
    && chown switchtender:switchtender /data

COPY --from=build /switchtender /usr/local/bin/switchtender
USER 10001:10001
WORKDIR /data
EXPOSE 8080
ENTRYPOINT ["switchtender"]
CMD ["serve", "--addr", ":8080"]
