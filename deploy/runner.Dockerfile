FROM golang:1.26.5-bookworm AS build
ARG GOPROXY=https://proxy.golang.org,direct
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /runner-controller ./cmd/runner-controller

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /runner-controller /runner-controller
USER 1000:1000
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=6s --start-period=10s CMD ["/runner-controller", "healthcheck"]
ENTRYPOINT ["/runner-controller"]
