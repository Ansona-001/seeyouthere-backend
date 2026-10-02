FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api
RUN mkdir -p /out/data/media

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /api
COPY --from=build --chown=65532:65532 /out/data /data
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/api"]
