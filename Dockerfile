# Orchestrator: build the web UI (Vite), build the Go binary with the embedded UI.
FROM node:22-bookworm-slim@sha256:de170b39439eb0e5a3444ad7d60b015614a381e10fecae2e30ec9e735122f38d AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af AS build
# X8.1: the identity of this build goes into the binary; the deploy sets it from the submodule state
# (`git describe --always --dirty` and the commit). Without arguments a local build reports "dev".
ARG AGW_COMMIT=dev
ARG AGW_DESCRIBE=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X agw/internal/version.Commit=${AGW_COMMIT} -X agw/internal/version.Describe=${AGW_DESCRIBE}" \
      -o /out/orchestrator ./cmd/orchestrator
# X1.4: the experiment runner runs in this image (docker exec -d … /agw exp run …); it needs no shell.
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X agw/internal/version.Commit=${AGW_COMMIT} -X agw/internal/version.Describe=${AGW_DESCRIBE}" \
      -o /out/agw ./cmd/agw

# Runs as root because it serves the Docker socket and the socket volume.
FROM gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
COPY --from=build /out/orchestrator /orchestrator
COPY --from=build /out/agw /agw
# X8.2: the skills the slots of this gateway run with, so that GET /api/exp/environment can state their
# checksum. The images for pi and the execution sandbox copy the same directory.
COPY --from=build /src/images/agw-basis/skills /etc/agw/skills
EXPOSE 18480 18481
ENTRYPOINT ["/orchestrator"]
