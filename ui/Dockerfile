# Build the static export, then serve it with Caddy (which also reverse-proxies
# /v1 and /healthz to the gateway). No Node runtime ships in the final image.
FROM node:20-alpine AS build
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm install --no-audit --no-fund
COPY . .
RUN npm run build

FROM caddy:2-alpine
COPY Caddyfile /etc/caddy/Caddyfile
COPY --from=build /app/out /srv
EXPOSE 80
