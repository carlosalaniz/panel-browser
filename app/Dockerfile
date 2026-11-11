FROM node:24-bookworm-slim

ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    NODE_ENV=production \
    DATA_DIR=/data \
    STREAM_PORT=8080 \
    SETTINGS_PORT=8081

WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund \
 && npx playwright-core install --with-deps --only-shell chromium \
 && apt-get install -y --no-install-recommends fonts-dejavu-core fonts-noto-core fonts-noto-color-emoji \
 && rm -rf /var/lib/apt/lists/* /root/.npm \
 && mkdir -p /data && chown node:node /data

COPY server.mjs ./
COPY web ./web

USER node
VOLUME /data
EXPOSE 8080 8081
CMD ["node", "server.mjs"]
