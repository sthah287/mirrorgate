# Built from the repo root because the worker depends on the shared module.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY shared/go.mod shared/go.sum ./shared/
COPY comparison-worker/go.mod comparison-worker/go.sum ./comparison-worker/
WORKDIR /src/comparison-worker
RUN go mod download
COPY shared/ /src/shared/
COPY comparison-worker/ /src/comparison-worker/
RUN CGO_ENABLED=0 go build -o /bin/comparison-worker ./cmd/worker

FROM alpine:3.21
COPY --from=build /bin/comparison-worker /usr/local/bin/comparison-worker
CMD ["comparison-worker"]
