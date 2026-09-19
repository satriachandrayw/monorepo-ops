FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/resilient-git .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates git openssh-client
COPY --from=build /out/resilient-git /usr/local/bin/resilient-git
ENTRYPOINT ["/usr/local/bin/resilient-git"]
