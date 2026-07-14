# Build the single static binary, then run it on a small image that carries ansible.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /railwarden .

FROM alpine:3.20
RUN apk add --no-cache ansible-core openssh-client ca-certificates
COPY --from=build /railwarden /usr/local/bin/railwarden
EXPOSE 8080
ENTRYPOINT ["railwarden"]
CMD ["serve", "--addr", ":8080"]
