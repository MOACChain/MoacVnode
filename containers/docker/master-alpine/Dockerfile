FROM alpine:3.5

RUN \
  apk add --update go git make gcc musl-dev linux-headers ca-certificates && \
  git clone --depth 1 --branch release/1.6 https://github.com/MOACChain/MoacVnode && \
  (cd MOAC-core && make geth) && \
  cp MOAC-core/build/bin/geth /geth && \
  apk del go git make gcc musl-dev linux-headers && \
  rm -rf /MOAC-core && rm -rf /var/cache/apk/*

EXPOSE 8545
EXPOSE 30303

ENTRYPOINT ["/geth"]
