# syntax=docker/dockerfile:1
# [[.Name]] · statik site (nginx, root olmadan, port 8080) · FootOps "Projeni hazırla" ile üretildi.
# GitHub Actions bu dosyayla imajı derleyip [[.Image]] adıyla GHCR'ye gönderir;
# sunucu (FootOps) imajı oradan çeker.
[[- if .BuildDir]]

FROM node:24-alpine AS builder
WORKDIR /app
COPY package.json [[.Lock]] .npmrc* ./
RUN [[.Install]]
COPY . .
ENV NODE_ENV=production
RUN [[.Build]]

FROM nginxinc/nginx-unprivileged:stable-alpine
# Derlemenin çıktısı ([[.BuildDir]]) yayınlanır
COPY --from=builder /app/[[.BuildDir]] /usr/share/nginx/html
[[- else]]

FROM nginxinc/nginx-unprivileged:stable-alpine
# Proje klasörü olduğu gibi yayınlanır (.dockerignore'dakiler hariç)
COPY . /usr/share/nginx/html
[[- end]]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null "http://127.0.0.1:8080/" || exit 1
