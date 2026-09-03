FROM golang:1.26-alpine AS build
WORKDIR /src
COPY sail/go.mod sail/go.sum ./
RUN go mod download
COPY sail/ ./
RUN CGO_ENABLED=0 go build -o /out/sailnode ./cmd/sailnode && CGO_ENABLED=0 go build -o /out/sail ./cmd/sail

FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl
COPY --from=build /out/sailnode /out/sail /usr/local/bin/
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENV SAIL_HOME=/data
ENTRYPOINT ["/entrypoint.sh"]
