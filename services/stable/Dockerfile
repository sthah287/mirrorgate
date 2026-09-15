FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /bin/stable .

FROM alpine:3.21
COPY --from=build /bin/stable /usr/local/bin/stable
EXPOSE 9001
CMD ["stable"]
