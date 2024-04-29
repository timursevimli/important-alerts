# # syntax=docker/dockerfile:1
#
# FROM golang:1.22-alpine
# WORKDIR /app
# COPY go.mod go.sum ./
# RUN go mod download
# COPY *.go ./
# RUN CGO_ENABLED=0 GOOS=linux go build -o /important-notifications
# CMD ["/important-notifications"]


FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /important-notifications

FROM alpine:latest
COPY . .
COPY --from=builder /important-notifications /important-notifications
CMD ["/important-notifications"]
