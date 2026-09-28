# Built from the repo root because the service uses the shared tracing setup.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY shared/go.mod shared/go.sum ./shared/
COPY services/candidate/go.mod ./services/candidate/
WORKDIR /src/services/candidate
RUN go mod download
COPY shared/ /src/shared/
COPY services/candidate/ /src/services/candidate/
RUN CGO_ENABLED=0 go build -o /bin/candidate .

FROM alpine:3.21
COPY --from=build /bin/candidate /usr/local/bin/candidate
CMD ["candidate"]
