# Build: docker build -t scalebridge-sync .
# Run:   docker run -d --name scalebridge-sync --restart unless-stopped \
#          -p 127.0.0.1:8723:8723 -v scalebridge-sync:/data scalebridge-sync
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /scalebridge-sync .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 10001 scalebridge \
    && mkdir /data && chown scalebridge /data
COPY --from=build /scalebridge-sync /usr/local/bin/scalebridge-sync
USER scalebridge
VOLUME /data
EXPOSE 8723
LABEL org.opencontainers.image.source="https://github.com/ulfdalen/scalebridge-sync" \
      org.opencontainers.image.description="Sync your Withings scale to Garmin Connect, from your own computer." \
      org.opencontainers.image.licenses="MIT"
# 0.0.0.0 inside the container so Docker's port mapping can reach it; publish
# to the host loopback only (-p 127.0.0.1:8723:8723) — the UI has no login.
ENTRYPOINT ["scalebridge-sync", "run", "--config", "/data", "--bind", "0.0.0.0"]
