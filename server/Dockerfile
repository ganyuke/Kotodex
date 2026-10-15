# Cross-compiles on the build machine, so multi-platform images build fast
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /kotodex . && mkdir -p /data/manga

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /kotodex /kotodex
COPY --from=build --chown=65532:65532 /data /data
ENV KOTODEX_DATA_DIR=/data KOTODEX_LISTEN=:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/kotodex"]
