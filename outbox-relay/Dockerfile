# Built from the repo root because the relay depends on the shared module.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY shared/go.mod shared/go.sum ./shared/
COPY outbox-relay/go.mod ./outbox-relay/
WORKDIR /src/outbox-relay
RUN go mod download
COPY shared/ /src/shared/
COPY outbox-relay/ /src/outbox-relay/
RUN CGO_ENABLED=0 go build -o /bin/outbox-relay ./cmd/relay

FROM alpine:3.21
COPY --from=build /bin/outbox-relay /usr/local/bin/outbox-relay
CMD ["outbox-relay"]
