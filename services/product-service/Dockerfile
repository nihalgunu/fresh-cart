FROM golang:1.23-alpine AS builder
WORKDIR /app
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /product-service .

FROM alpine:3.19
RUN addgroup -g 1000 appgroup && adduser -u 1000 -G appgroup -D appuser
WORKDIR /app
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /product-service .
USER appuser
EXPOSE 8082
ENTRYPOINT ["./product-service"]
