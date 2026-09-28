# Built from the repo root because the service uses the shared tracing setup.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY shared/go.mod shared/go.sum ./shared/
COPY services/stable/go.mod ./services/stable/
WORKDIR /src/services/stable
RUN go mod download
COPY shared/ /src/shared/
COPY services/stable/ /src/services/stable/
RUN CGO_ENABLED=0 go build -o /bin/stable .

FROM alpine:3.21
COPY --from=build /bin/stable /usr/local/bin/stable
CMD ["stable"]
