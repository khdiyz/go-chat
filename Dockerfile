# Dockerfile
FROM golang:1.20-alpine AS builder

WORKDIR /app

# Copy Go module files and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o chat-app .

# Create final image
FROM alpine:latest

WORKDIR /app

# Install required packages
RUN apk --no-cache add ca-certificates

# Copy built executable from builder stage
COPY --from=builder /app/chat-app /app/
COPY --from=builder /app/static /app/static

EXPOSE 7776

CMD ["./chat-app"]