FROM node:22-alpine AS frontend
WORKDIR /src/frontend
COPY frontend/package*.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN rm -rf ./web/dist && mkdir -p ./web/dist
COPY --from=frontend /src/frontend/dist/ ./web/dist/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/router ./cmd/router
RUN mkdir -p /out/data && chown 65532:65532 /out/data

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/router /app/router
COPY --from=build --chown=nonroot:nonroot /out/data /app/data
COPY config.example.yaml /app/config.example.yaml
EXPOSE 8080
USER nonroot:nonroot
ENV CONFIG_FILE=/app/config.example.yaml
ENV ROUTER_CONTAINER_PORT=8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 CMD ["/app/router", "healthcheck"]
ENTRYPOINT ["/app/router"]
