# Orchestrator: Web-UI (Vite) bauen, Go-Binary mit eingebetteter UI bauen.
FROM node:22-bookworm-slim AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/orchestrator ./cmd/orchestrator

# Läuft als root, weil es den Docker-Socket und das Socket-Volume bedient.
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/orchestrator /orchestrator
EXPOSE 18480 18481
ENTRYPOINT ["/orchestrator"]
