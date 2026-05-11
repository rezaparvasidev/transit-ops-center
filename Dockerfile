# syntax=docker/dockerfile:1.6

# ---- Stage 1: build SvelteKit static site ----
FROM node:20-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json* ./
RUN npm install --no-audit --no-fund
COPY web/ ./
RUN npm run build

# ---- Stage 2: build Go binary ----
FROM golang:1.22-alpine AS api
RUN apk add --no-cache git ca-certificates
WORKDIR /src
COPY go.mod ./
COPY services/ ./services/
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags='-s -w' -o /out/api ./services/api

# ---- Stage 3: runtime ----
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=api /out/api /app/api
COPY --from=web /web/build /app/web/build
ENV PORT=8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/api"]
