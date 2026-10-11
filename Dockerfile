# Build a static binary, then ship it on Alpine with ffmpeg.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /aural .

FROM alpine:3.20
RUN apk add --no-cache ffmpeg ca-certificates tzdata
COPY --from=build /aural /usr/local/bin/aural
ENV AURAL_ADDR=:8078 AURAL_DATA=/data
VOLUME /data
EXPOSE 8078
ENTRYPOINT ["aural"]
