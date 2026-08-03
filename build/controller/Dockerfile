# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.26.2

FROM golang:${GO_VERSION} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/controller-manager ./cmd/controller-manager

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/controller-manager /controller-manager
USER nonroot:nonroot
ENTRYPOINT ["/controller-manager"]
