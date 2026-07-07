# Build the single static binary, then run it on a small image that carries ansible.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /yardmaster .

FROM alpine:3.20
RUN apk add --no-cache ansible-core openssh-client ca-certificates
COPY --from=build /yardmaster /usr/local/bin/yardmaster
EXPOSE 8080
ENTRYPOINT ["yardmaster"]
CMD ["serve", "--addr", ":8080"]
