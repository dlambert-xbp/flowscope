# syntax=docker/dockerfile:1.7
#
# Two-stage Vite build: install + build the React SPA, then serve the
# static output via a tiny nginx. Same-origin proxy for /api, /metrics
# and /healthz to the FlowScope api service.

FROM node:22-alpine AS build
WORKDIR /src
COPY package.json ./
# We don't ship a lockfile yet — slice 8 is the bootstrap. A follow-up
# slice locks it. `npm install` works either way.
RUN npm install --no-audit --no-fund --loglevel=error
COPY tsconfig.json tsconfig.node.json vite.config.ts index.html ./
COPY src ./src
RUN npm run build

FROM nginx:1.27-alpine AS runtime
COPY --from=build /src/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 80
