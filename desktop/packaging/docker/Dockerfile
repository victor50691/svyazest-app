# «Связь Есть?» — агент в контейнере. Бинарники собраны заранее (build.sh
# кладёт их в bin/<arch>/ контекста), внутри образа ничего не компилируется.
# Сеть — хоста (network_mode: host): агенту нужны настоящие интерфейсы,
# модем и Ethernet, а не сеть Docker.
FROM debian:stable-slim

ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=1.0.1

# mmcli (пакет modemmanager) спрашивает ModemManager ХОСТА через
# смонтированный сокет D-Bus; сам демон в контейнере не запускается.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates modemmanager iproute2 tzdata \
 && rm -rf /var/lib/apt/lists/*

COPY bin/${TARGETARCH}${TARGETVARIANT}/ /usr/lib/svyazest-agent/
RUN ln -s /usr/lib/svyazest-agent/svyazest-agent /usr/bin/svyazest-agent \
 && mkdir -p /data && chmod 0700 /data

ENV SVYAZEST_DATA=/data
VOLUME ["/data"]
EXPOSE 8780

LABEL org.opencontainers.image.title="svyazest-agent" \
      org.opencontainers.image.description="Связь Есть? — агент проверок DPI//Checker через мобильный модем" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.url="https://github.com/victor50691/svyazest-app/releases/tag/desktop"

ENTRYPOINT ["/usr/lib/svyazest-agent/svyazest-agent"]
CMD ["run", "--data-dir", "/data"]
