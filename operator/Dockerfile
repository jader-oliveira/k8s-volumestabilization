FROM golang:1.21-alpine AS builder

WORKDIR /workspace

# Install git
RUN apk add --no-cache git

# Copy everything
COPY . .

# Get deps and build
RUN go mod tidy && go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -o manager main.go

# Final image
FROM alpine:latest
RUN apk --no-cache add ca-certificates && \
    addgroup -g 65532 nonroot && \
    adduser -u 65532 -G nonroot -s /bin/sh -D nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]