FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/mirrorgate ./cmd/mirrorgate

FROM alpine:3.21
COPY --from=build /bin/mirrorgate /usr/local/bin/mirrorgate
EXPOSE 8080 8081
CMD ["mirrorgate"]
