FROM golang:1.23-alpine AS builder
WORKDIR /workspace

COPY ./ ./
RUN go build -o chaturbate-dvr .

FROM alpine AS runnable
RUN apk add --no-cache ffmpeg ca-certificates
WORKDIR /usr/src/app

COPY --from=builder /workspace/chaturbate-dvr /chaturbate-dvr

ENTRYPOINT ["/chaturbate-dvr"]
