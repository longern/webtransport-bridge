FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN go mod tidy && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /bridge ./cmd/bridge && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /certgen ./cmd/certgen
FROM alpine:3.23
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 bridge
COPY --from=build /bridge /certgen /usr/local/bin/
USER 10001
EXPOSE 4433/udp
ENTRYPOINT ["bridge"]
