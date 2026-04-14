# Build stage
FROM golang:1.24.5-alpine AS builder

WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY src/ ./src/

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o runner ./src/main.go

# Final stage
FROM alpine:3.18

# Install required tools: tc (iproute2) and nsenter (util-linux)
RUN apk add --no-cache iproute2 util-linux

WORKDIR /app

# Copy the binary from the builder stage
COPY --from=builder /app/runner .

# Expose the port the app runs on
EXPOSE 8080

# Run the application
CMD ["./runner"]
