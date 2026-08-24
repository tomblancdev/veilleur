# Le Veilleur — one static binary, nothing else in the image.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/veilleur ./cmd/veilleur

FROM scratch
LABEL org.opencontainers.image.source="https://github.com/tomblancdev/veilleur" \
      org.opencontainers.image.description="Le Veilleur — the watchman: machines are awake exactly as long as somebody has claimed them" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/veilleur /veilleur
VOLUME ["/data"]
USER 65532:65532
EXPOSE 8080
ENV VEILLEUR_CONFIG=/etc/veilleur/config.yaml VEILLEUR_DATA_DIR=/data
ENTRYPOINT ["/veilleur"]
