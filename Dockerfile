FROM ghcr.io/matrix-construct/tuwunel:v1.9.2@sha256:a354976db35cb6e3f8c0b801f266592571af898554d78fc6e2137f14e02a7d3e AS matrix
FROM vectorim/element-web:v1.12.13@sha256:5107e63026c13ed014f743e485821b7d4b56d275a41e76303859bb14f5f94eb6 AS element
FROM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build
WORKDIR /src
COPY go.mod *.go ./
RUN GOMEMLIMIT=512MiB GOFLAGS=-p=2 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /bridge .

FROM docker:29.9.1-cli@sha256:6fae2214c3121a28d5aa639d09032f61310b7c9c75ffbad1025f261a1b621800 AS docker-cli

FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587 AS speech
RUN test "$(dpkg --print-architecture)" = amd64
RUN apt-get update && apt-get install -y --no-install-recommends bzip2 ca-certificates && rm -rf /var/lib/apt/lists/*
ADD --checksum=sha256:cb9efc3014d66b4c68f602db2447ac0fc847d6b6e9152216e12ccb6b6e4c4d10 https://github.com/k2-fsa/sherpa-onnx/releases/download/v1.13.8/sherpa-onnx-v1.13.8-linux-x64-static-no-tts.tar.bz2 /tmp/sherpa.tar.bz2
ADD --checksum=sha256:157c157bc51155e03e37d2466522a3a737dd9c72bb25f36eb18912964161e1ad https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8.tar.bz2 /tmp/parakeet.tar.bz2
RUN mkdir /speech /model && tar -xjf /tmp/sherpa.tar.bz2 -C /speech --strip-components=2 sherpa-onnx-v1.13.8-linux-x64-static-no-tts/bin/sherpa-onnx-offline && tar -xjf /tmp/parakeet.tar.bz2 -C /model --strip-components=1 --wildcards '*/encoder.int8.onnx' '*/decoder.int8.onnx' '*/joiner.int8.onnx' '*/tokens.txt'

FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates ffmpeg util-linux && rm -rf /var/lib/apt/lists/*
COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker-cli /usr/local/libexec/docker/cli-plugins/docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose
COPY --from=speech /speech/sherpa-onnx-offline /usr/local/bin/sherpa-onnx-offline
COPY --from=speech /model /opt/parakeet
COPY --from=matrix /usr/bin/tuwunel /usr/bin/tuwunel
COPY --from=element /app /opt/element
COPY --from=build /bridge /usr/local/bin/bridge
COPY THIRD_PARTY_NOTICES.md /opt/THIRD_PARTY_NOTICES.md
COPY THIRD_PARTY_NOTICES.md /opt/element/licenses/THIRD_PARTY_NOTICES.md
COPY --from=build /usr/local/go/LICENSE /opt/element/licenses/Go-LICENSE
ADD --chmod=0444 --checksum=sha256:a8ccad6d8ae08bf7d78cebffafb6c5e4e70b35a9fec705a52fd137674e1d1df7 https://raw.githubusercontent.com/matrix-construct/tuwunel/v1.9.2/LICENSE /opt/element/licenses/Tuwunel-LICENSE
ADD --chmod=0444 --checksum=sha256:5132c7f0475b02c8107a2e0f0363e70423c62d2664ab927e76193226e5e05905 https://raw.githubusercontent.com/element-hq/element-web/v1.12.13/LICENSE-AGPL-3.0 /opt/element/licenses/Element-AGPL-3.0
ADD --chmod=0444 --checksum=sha256:8b1ba204bb69a0ade2bfcf65ef294a920f6bb361b317dba43c7ef29d96332b9b https://raw.githubusercontent.com/element-hq/element-web/v1.12.13/LICENSE-GPL-3.0 /opt/element/licenses/Element-GPL-3.0
ADD --chmod=0444 --checksum=sha256:facf86d2b258aa8ed5fad981fadff45eb4d7d0a6568e0babec5f127603e6df4b https://raw.githubusercontent.com/element-hq/element-web/v1.12.13/AUTHORS.rst /opt/element/licenses/Element-AUTHORS.rst
ADD --chmod=0444 --checksum=sha256:cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30 https://raw.githubusercontent.com/k2-fsa/sherpa-onnx/v1.13.8/LICENSE /opt/element/licenses/Sherpa-onnx-LICENSE
ADD --chmod=0444 --checksum=sha256:2f07c72751aed99790b8a4869cf2311df85a860b22ded05fa22803587a48922c https://raw.githubusercontent.com/microsoft/onnxruntime/v1.28.2/LICENSE /opt/element/licenses/ONNX-Runtime-LICENSE
ADD --chmod=0444 --checksum=sha256:0e07b95f3a8d6230037707c5c4a2b554d12c4cb67369669ac255635528ffcee2 https://raw.githubusercontent.com/microsoft/onnxruntime/v1.28.2/ThirdPartyNotices.txt /opt/element/licenses/ONNX-Runtime-ThirdPartyNotices.txt
ADD --chmod=0444 --checksum=sha256:9ba9550ad48438d0836ddab3da480b3b69ffa0aac7b7878b5a0039e7ab429411 https://raw.githubusercontent.com/creativecommons/creativecommons.org/main/docroot/legalcode/by_4.0.txt /opt/element/licenses/Parakeet-CC-BY-4.0.txt
ENV HOME=/tmp
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/bridge"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=30m --retries=3 CMD ["/usr/local/bin/bridge", "health"]
