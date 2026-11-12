# syntax=docker/dockerfile:1
# [[.Name]] · Next.js (standalone) · FootOps "Projeni hazırla" ile üretildi.
# GitHub Actions bu dosyayla imajı derleyip [[.Image]] adıyla GHCR'ye gönderir;
# sunucu (FootOps) imajı oradan çeker. next.config'de output: 'standalone' olmalı.

FROM node:24-alpine AS deps
WORKDIR /app
COPY package.json [[.Lock]] .npmrc* ./
RUN [[.Install]]

FROM node:24-alpine AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
ENV NODE_ENV=production NEXT_TELEMETRY_DISABLED=1
RUN [[.Build]]

FROM node:24-alpine AS runner
WORKDIR /app
# HOSTNAME=0.0.0.0 şart: yoksa Next yalnız container'ın içinden erişilebilir dinler
ENV NODE_ENV=production \
    NEXT_TELEMETRY_DISABLED=1 \
    TZ=Europe/Istanbul \
    PORT=[[.Port]] \
    HOSTNAME=0.0.0.0
RUN addgroup -S -g 1001 app && adduser -S -D -H -u 1001 -G app app
COPY --from=builder /app/.next/standalone ./
COPY --from=builder /app/.next/static ./.next/static
COPY --from=builder /app/public ./public
[[- if .DataDir]]
# Kod root'un ve salt okunur. Uygulama yalnız görsel önbelleğine ve yükleme
# klasörüne yazar; sunucuda kalıcı klasör [[.DataDir]] yoluna bağlanır.
RUN mkdir -p .next/cache [[.DataDir]] && chown app:app .next/cache [[.DataDir]]
LABEL footops.data-dir="[[.DataDir]]"
[[- else]]
# Kod root'un ve salt okunur; uygulama yalnız görsel önbelleğine yazar.
RUN mkdir -p .next/cache && chown app:app .next/cache
[[- end]]
USER app
EXPOSE [[.Port]]
# busybox wget: her kontrolde ayrı bir node süreci başlatmaz
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD wget -q -O /dev/null "http://127.0.0.1:[[.Port]]/" || exit 1
CMD ["node", "server.js"]
