FROM golang:1.12 as build

# Install upx to compress executable after compilation
RUN apt-get update && \
    apt-get install -y xz-utils && \
    mkdir -p /opt/upx && \
    cd /opt/upx && \
    wget -q https://github.com/upx/upx/releases/download/v3.95/upx-3.95-amd64_linux.tar.xz -O ./upx.tar.xz && \
    tar --strip-components 1 -xf upx.tar.xz && \
    rm -rf /var/lib/apt/lists/* upx.tar.xz

# Install modules first for caching
WORKDIR /app
ENV GO111MODULE=on
COPY go.* ./
RUN go mod download

# Build the application, and compress with upx
ARG VERSION
COPY ./ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w -X main.version=${VERSION:-0.0.0}" && \
    /opt/upx/upx shawarma-webhook



# Copy compiled output to a fresh image
FROM scratch
WORKDIR /etc/shawarma-webhook

COPY --from=build ["/app/shawarma-webhook", "/app/sidecar.yaml", "./"]

# Ensure the tmp folder is available
VOLUME [ "/tmp", "/etc/shawarma-webhook/certs" ]

ENV CERT_FILE=/etc/shawarma-webhook/certs/cert.pem \
    KEY_FILE=/etc/shawarma-webhook/certs/key.pem \
    WEBHOOK_PORT=443 \
    SHAWARMA_IMAGE=centeredge/shawarma:0.1.2 \
    SHAWARMA_SECRET_TOKEN_NAME=shawarma-token \
    LOG_LEVEL=warn

ENTRYPOINT [ "/etc/shawarma-webhook/shawarma-webhook" ]
CMD []
