# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -o /out/verify ./cmd/verify

FROM alpine:3.21
COPY --from=build /out/server /usr/local/bin/server
COPY --from=build /out/verify /usr/local/bin/verify
EXPOSE 8080
CMD ["server"]
