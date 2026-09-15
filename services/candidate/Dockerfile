FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /bin/candidate .

FROM alpine:3.21
COPY --from=build /bin/candidate /usr/local/bin/candidate
EXPOSE 9002
CMD ["candidate"]
