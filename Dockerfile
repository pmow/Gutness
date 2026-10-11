# Build a static binary, then ship it on Alpine with ffmpeg.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /gutness .

FROM alpine:3.20
RUN apk add --no-cache ffmpeg ca-certificates tzdata
COPY --from=build /gutness /usr/local/bin/gutness
# GUTNESS_PORT is the only environment setting; everything else is in the web UI.
ENV GUTNESS_PORT=8078
VOLUME /data
EXPOSE 8078
ENTRYPOINT ["gutness"]
