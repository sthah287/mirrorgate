# Built from the repo root because the gateway depends on the shared module.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY shared/go.mod shared/go.sum ./shared/
COPY gateway/go.mod gateway/go.sum ./gateway/
WORKDIR /src/gateway
RUN go mod download
COPY shared/ /src/shared/
COPY gateway/ /src/gateway/
RUN CGO_ENABLED=0 go build -o /bin/mirrorgate ./cmd/mirrorgate

FROM alpine:3.21
COPY --from=build /bin/mirrorgate /usr/local/bin/mirrorgate
EXPOSE 8080 8081
CMD ["mirrorgate"]
