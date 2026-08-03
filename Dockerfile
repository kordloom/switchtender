# Build the single static binary, then run it on a small image that carries ansible.
# The build stage runs on the build platform and cross-compiles, so multi-arch images
# never compile Go under emulation.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/kordloom/switchtender/cmd.Version=$VERSION" -o /switchtender .

FROM alpine:3.20
RUN apk add --no-cache ansible-core openssh-client ca-certificates
COPY --from=build /switchtender /usr/local/bin/switchtender
EXPOSE 8080
ENTRYPOINT ["switchtender"]
CMD ["serve", "--addr", ":8080"]
