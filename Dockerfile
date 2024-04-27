# syntax=docker/dockerfile:1

FROM golang:1.22-alpine
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /important-notifications
CMD ["/important-notifications"]
