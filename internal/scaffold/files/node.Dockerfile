# syntax=docker/dockerfile:1
# [[.Name]] · Node.js · FootOps "Projeni hazırla" ile üretildi.
# GitHub Actions bu dosyayla imajı derleyip [[.Image]] adıyla GHCR'ye gönderir;
# sunucu (FootOps) imajı oradan çeker. Uygulama process.env.PORT'u dinlemeli.

FROM node:24-alpine
WORKDIR /app
ENV NODE_ENV=production \
    TZ=Europe/Istanbul \
    PORT=[[.Port]]
COPY package.json [[.Lock]] .npmrc* ./
RUN [[.InstallProd]]
COPY . .
RUN addgroup -S -g 1001 app && adduser -S -D -H -u 1001 -G app app
[[- if .DataDir]]
# Kod root'un ve salt okunur. Uygulama yalnız yükleme klasörüne yazar;
# sunucuda kalıcı klasör [[.DataDir]] yoluna bağlanır.
RUN mkdir -p [[.DataDir]] && chown app:app [[.DataDir]]
LABEL footops.data-dir="[[.DataDir]]"
[[- else]]
# Kod root'un ve salt okunur: uygulama dosya yazamaz (yükleme varsa kalıcı klasör ver).
[[- end]]
USER app
EXPOSE [[.Port]]
# Sağlık kontrolü / adresine bakar; uygulamanda / yoksa buradaki yolu değiştir
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD wget -q -O /dev/null "http://127.0.0.1:[[.Port]]/" || exit 1
CMD ["node", "[[.Start]]"]
