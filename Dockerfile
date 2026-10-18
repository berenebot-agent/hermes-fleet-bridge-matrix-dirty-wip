FROM ghcr.io/matrix-construct/tuwunel:v1.9.2@sha256:a354976db35cb6e3f8c0b801f266592571af898554d78fc6e2137f14e02a7d3e AS matrix
FROM vectorim/element-web:v1.12.13@sha256:5107e63026c13ed014f743e485821b7d4b56d275a41e76303859bb14f5f94eb6 AS element
FROM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build
WORKDIR /src
COPY go.mod *.go ./
RUN GOMEMLIMIT=512MiB GOFLAGS=-p=2 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /bridge .

FROM docker:29.9.1-cli@sha256:6fae2214c3121a28d5aa639d09032f61310b7c9c75ffbad1025f261a1b621800
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
ENV HOME=/tmp
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/bridge"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=30m --retries=3 CMD ["/usr/local/bin/bridge", "health"]
