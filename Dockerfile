# Prebuilt TDLib matching go-tdlib v1.0.0-beta1 (TDLib commit 49b3bcb).
# Maintained by the go-tdlib author, so the td_api schemas line up.
FROM ghcr.io/zelenin/tdlib-docker:b498497-alpine AS tdlib

FROM golang:1.24.1-alpine3.21 AS go-builder

RUN apk add --no-cache build-base git ca-certificates openssl-dev zlib-dev linux-headers

WORKDIR /src

# TDLib headers and (static) libraries from the prebuilt image.
COPY --from=tdlib /usr/local/include/td /usr/local/include/td/
COPY --from=tdlib /usr/local/lib/libtd* /usr/local/lib/

COPY . /src

# go-tdlib links libtdjson statically by default (see client/tdjson_static.go).
RUN go mod tidy && \
    go build -trimpath -ldflags "-s -w" -o /out/gateway ./cmd/gateway && \
    go build -trimpath -ldflags "-s -w" -o /out/worker ./cmd/worker && \
    go build -trimpath -ldflags "-s -w" -o /out/smoke ./cmd/smoke

FROM alpine:3.21

RUN apk add --no-cache ca-certificates libstdc++ openssl zlib

WORKDIR /app
COPY --from=go-builder /out/gateway .
COPY --from=go-builder /out/worker .
COPY --from=go-builder /out/smoke .

EXPOSE 8080
CMD ["./gateway"]
