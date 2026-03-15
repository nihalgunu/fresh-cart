FROM golang:1.23-alpine AS builder
WORKDIR /app
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /order-service .

# Security scan stage
FROM aquasec/trivy:latest AS scanner
COPY --from=builder /order-service /order-service
RUN trivy rootfs --no-progress --severity HIGH,CRITICAL --exit-code 0 /

FROM alpine:3.19
RUN addgroup -g 1000 appgroup && adduser -u 1000 -G appgroup -D appuser
WORKDIR /app
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /order-service .
USER appuser
EXPOSE 8083
ENTRYPOINT ["./order-service"]
