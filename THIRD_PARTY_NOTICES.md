# Bundled third-party software

The image includes unmodified upstream components. Their copyright and licence
notices remain with the bundled assets. The controller also includes pinned Go
modules for Matrix E2EE and embedded SQLite. Their licence files and complete
module/version inventory are shipped under `/opt/element/licenses/go-modules/`.
Unmodified corresponding module source can be obtained with
`go mod download -json <module>@<version>` using that inventory (or through the
source links on `https://pkg.go.dev/<module>@<version>`). Preserve these source
availability notices when redistributing MPL-covered code.

- mautrix-go 0.31.0, including goolm — MPL-2.0, with inherited Apache-2.0 files:
  https://github.com/mautrix/go/tree/v0.31.0
- modernc.org/sqlite 1.38.2 — BSD-3-Clause wrapper and public-domain SQLite:
  https://pkg.go.dev/modernc.org/sqlite@v1.38.2
- go.mau.fi/util and other transitive Go modules — their individual notices are
  included in the generated Go-module licence directory; exact pins are in
  `go.mod`, `go.sum` and the image's `modules.txt`.

- Tuwunel 1.9.2 — Apache-2.0. Source and notices:
  https://github.com/matrix-construct/tuwunel/tree/v1.9.2
- Element Web 1.12.13 — AGPL-3.0-only (with upstream commercial licensing options).
  Corresponding source: https://github.com/element-hq/element-web/tree/v1.12.13
  The upstream `/app` asset tree, including its licences, is copied unmodified.
- Docker CLI / Compose — Apache-2.0; packaged by the upstream Docker CLI image.
  https://github.com/docker/cli and https://github.com/docker/compose
- Go standard library/runtime — BSD-3-Clause:
  https://go.dev/LICENSE
- Debian Bookworm base, FFmpeg, util-linux and dependencies — individual upstream
  licences, retained in `/usr/share/doc/*/copyright` in the image. Debian FFmpeg
  includes GPL components; preserve corresponding-source obligations when
  redistributing. Package source: https://sources.debian.org/ and
  https://deb.debian.org/debian/pool/ (match the installed package version).
- sherpa-onnx 1.13.8 — Apache-2.0; standalone CPU recognizer, unmodified:
  https://github.com/k2-fsa/sherpa-onnx/tree/v1.13.8
  This statically links ONNX Runtime 1.28.2 (MIT) and upstream native dependencies.
  Bundled engine/runtime licence and ONNX third-party notices are under
  `/opt/element/licenses/`.
- NVIDIA Parakeet TDT 0.6B v2 — CC-BY-4.0:
  https://huggingface.co/nvidia/parakeet-tdt-0.6b-v2
  NVIDIA is the original model author. The bundled INT8 ONNX conversion is from
  k2-fsa's sherpa-onnx ASR model release:
  https://github.com/k2-fsa/sherpa-onnx/releases/tag/asr-models
  Conversion/quantization changes the representation; this bridge makes no further
  changes to the model. No endorsement by NVIDIA or k2-fsa is implied. The full
  CC-BY-4.0 licence is included under `/opt/element/licenses/`.

Consult the upstream source releases for complete dependency notices. When
redistributing the image, preserve these notices and satisfy the licences of
all bundled components, including Element's corresponding-source obligations.
