# Multi-stage build for rc-mcp-server. See docs/specs/backend.md Section 14.
FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /rc-mcp-server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /rc-mcp-server /rc-mcp-server
EXPOSE 8080 9090
ENTRYPOINT ["/rc-mcp-server"]
