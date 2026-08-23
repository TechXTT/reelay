FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X github.com/TechXTT/reelay/internal/buildinfo.Version=${VERSION} -X github.com/TechXTT/reelay/internal/buildinfo.Commit=${COMMIT}" \
    -o /reelay ./cmd/reelay

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /reelay /reelay
VOLUME ["/config", "/data", "/media"]
EXPOSE 7878
ENTRYPOINT ["/reelay", "--config", "/config/config.yaml"]

