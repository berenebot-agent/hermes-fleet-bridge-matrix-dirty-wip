# Bundled third-party software

The image includes unmodified upstream components. Their copyright and licence
notices remain with the bundled assets. This controller uses only Go's standard
library; it does not include third-party Go modules.

- Tuwunel 1.9.2 — Apache-2.0. Source and notices:
  https://github.com/matrix-construct/tuwunel/tree/v1.9.2
- Element Web 1.12.13 — AGPL-3.0-only (with upstream commercial licensing options).
  Corresponding source: https://github.com/element-hq/element-web/tree/v1.12.13
  The upstream `/app` asset tree, including its licences, is copied unmodified.
- Docker CLI / Compose — Apache-2.0; packaged by the upstream Docker CLI image.
  https://github.com/docker/cli and https://github.com/docker/compose
- Go standard library/runtime — BSD-3-Clause:
  https://go.dev/LICENSE
- Alpine base packages — individual upstream licences:
  https://pkgs.alpinelinux.org/

Consult the upstream source releases for complete dependency notices. When
redistributing the image, preserve these notices and satisfy the licences of
all bundled components, including Element's corresponding-source obligations.
