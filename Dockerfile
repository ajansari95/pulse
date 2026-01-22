FROM golang:1.23-alpine AS builder
WORKDIR /app
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /pulse .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /pulse /pulse
COPY config.yaml /config.yaml
EXPOSE 8080
ENTRYPOINT ["/pulse", "/config.yaml"]
