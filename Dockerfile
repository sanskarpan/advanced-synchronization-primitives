# Multi-stage build: compile in full Go image, run in minimal Alpine.
FROM golang:1.21-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o syncprimitives-server ./cmd/server

# Runtime image: minimal Alpine, non-root user.
FROM alpine:3.19

RUN addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /app
COPY --from=builder /app/syncprimitives-server .

USER appuser

EXPOSE 8085

ENTRYPOINT ["/app/syncprimitives-server"]
CMD ["-addr", ":8085"]
