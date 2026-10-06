FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /bin/mock-external .

FROM alpine:3.21
COPY --from=build /bin/mock-external /usr/local/bin/mock-external
CMD ["mock-external"]
