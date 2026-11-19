# syntax=docker/dockerfile:1
# chatmux — one binary, many chat platforms, one brain.
# Build:  docker build -t chatmux .
# Run:    docker run -v $PWD/chatmux.json:/app/chatmux.json \
#         -v chatmux-data:/app/data -p 8080:8080 chatmux
FROM golang:1.21-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/chatmux .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 chatmux
COPY --from=build /out/chatmux /usr/local/bin/chatmux
USER chatmux
WORKDIR /app
VOLUME ["/app/data"]
EXPOSE 8080
ENTRYPOINT ["chatmux"]
CMD ["-config", "/app/chatmux.json"]
