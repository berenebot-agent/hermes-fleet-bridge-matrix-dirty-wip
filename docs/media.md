# Images, files and local voice transcription

This is a bridge-only extension. It does not install a native Matrix adapter in
Hermes, modify Hermes, or introduce another Compose service.

**E2EE amendment (2026-10-21):** [encryption.md](encryption.md) adds encrypted
message envelopes and Matrix v2 encrypted attachments. The transport now uses
mautrix-go and embedded SQLite; the file confinement and quota rules below remain.

## Inbound behaviour

- Accept `m.image`, `m.file` and `m.audio` messages. Apply existing room, mention,
  removed-agent and reserved-bot checks. New thread prompts remain unsupported.
- Each event is one turn. No album batching or delay to merge separately sent text.
- With Matrix `filename` present, `body` is the caption. Without it, `body` is the
  display filename, not a prompt. Names are display metadata, never storage paths.
- An `m.audio` message with `org.matrix.msc3245.voice` is a voice note. Unmarked
  audio is delivered as a file. The pinned client's actual upload/voice UI still
  needs a separate browser/mobile acceptance check.
- Accept local `mxc://<configured-server>/<media-id>` only. Download through the
  private homeserver's authenticated client media endpoint with `allow_remote=false`;
  refuse redirects and non-MXC URLs. Accept validated v2 encrypted `file` payloads,
  verify their ciphertext hash before decrypting/staging. Never fetch arbitrary
  attachment URLs or enable federation.
- Persist receipt/reference and the sync cursor together. Download to a generated
  temporary name, enforce the actual byte cap, fsync/rename, then persist checksum,
  detected type and size. A claimed MIME type or size is not trusted.
- Images must sniff as images. Generic files are not executed or unpacked by the
  bridge. Hermes sees a JSON-escaped display filename, detected MIME and local path;
  interpretation uses its existing vision/document/filesystem tools.
- Reject failures visibly without a Hermes run. Do not automatically re-admit a
  potentially executed instruction. A new upload is an explicit retry.

## Paths and permissions

The approved filesystem extension is limited to each configured member's existing
Hermes home (`data/`, mounted at `/opt/data` inside that member):

```text
<member>/data/matrix-media/
  incoming/<stable-event-hash>/file
  outgoing/<stable-event-hash>/<selected-filename>

<bridge>/data/controller/
  media/<agent-id>/<stable-event-hash>/<attachment-index>
  models/parakeet-v2-int8/{encoder.int8.onnx,decoder.int8.onnx,joiner.int8.onnx,tokens.txt}
```

The bridge creates directories/files with 0700/0600 permissions. The agent must
have access through the same member UID/permissions already required for its
persistent home. Storage operations use Go's confined `os.Root` APIs, refuse
symlink directories, and refuse symlink/non-regular/hard-linked selected files.
No new mount, named volume, Docker exec or agent-credential read path is added.

Private snapshots are outside the member's home; an agent cannot alter a committed
snapshot through its member mount. Each turn has its own outgoing folder, so a
reference cannot directly select another conversation's output. This is not an
agent-tool sandbox: an agent with filesystem tools can still copy a file itself.

## Outgoing protocol

The bridge supplies these instructions in each run input:

1. Create any desired output under the supplied turn-specific outgoing directory.
2. Select files in the **final reply**, one plain basename per line:

   ```text
   Here is the report.
   MATRIX_FILE: report.pdf
   MATRIX_FILE: chart.png
   ```

Only lines starting exactly `MATRIX_FILE: ` are attachment references. The marker
is bridge-specific, not a claim of native Hermes HTTP attachment support. No
markdown-link parsing, arbitrary absolute paths, subdirectories or directory
watching. The agent must follow the protocol; a path in ordinary prose stays text.

All selected files must pass validation before any are enqueued. Snapshot them
privately, reject known managed credentials rather than modifying binary contents,
then atomically enqueue final text and file notices with terminal run state.
Detected images use `m.image`; everything else uses `m.file`. Captions/final text
remain separate Matrix text messages.

The outbox saves each uploaded MXC URI before sending its room message. Retries
reuse that URI and the same message transaction ID. A crash after upload but before
saving its URI can produce a repeated/orphaned homeserver media object. A remote
transaction-cache expiry can still produce a duplicate visible message. Neither
case is permission to repeat the Hermes turn. After eight delivery failures,
`!resend` retries the existing outbox.

## Voice preprocessing

Use **sherpa-onnx 1.13.8 + NVIDIA Parakeet TDT 0.6B v2 INT8**, the English model
listed by OpenChamber. The approved Debian runtime supplies glibc; the recognizer
release labelled “static” still needs its glibc loader. Current packaging targets
Linux amd64 explicitly. No GPU, cloud request, Groq key, Node or Python is needed.

FFmpeg decodes an immutable, checksummed recording copy through pipes, with only
the `pipe` input protocol enabled, to 16 kHz mono signed PCM. Decode at most 301
seconds so recordings over the five-minute limit can be rejected rather than
silently truncated. Empty/digital-silence audio and unusable transcripts fail
visibly. Noise can still cause misrecognition; there is no reliable confidence or
general silence detector claimed here.

One recording is processed at a time across the fleet. Native commands have a
three-minute shared deadline, 120 CPU-second and 2 GiB address-space limits;
recognition uses two threads. Stdout/stderr are bounded and not logged or returned
on errors. The complete Compose service has a 3 GiB memory cap, leaving headroom
for the homeserver/controller alongside the speech subprocess. Decoded temporary
WAV files are private and removed after processing;
the maximum PCM payload is about 9.6 MB. The model loads per recording and exits
afterwards, trading cold latency for low idle memory. A five-minute duration cap
is not a promise that every recording fits the resource/time cap.

The image contains the pinned model. First voice use copies it into private
persistent storage; later uses verify its component checksums. A corrupt existing
cache is refused, not silently overwritten. No runtime model download is needed.

Persist the transcript as queued-turn text before contacting Hermes. Restarts
reuse that transcript and existing run-admission idempotency. Transcripts and
captions never enter the bridge-command dispatcher, even when they start with `!`.
Terminal turns scrub their transient prompt/transcript copies from controller
state, as text-only turns already do. Hermes retains its conversation normally.

Every transcribed voice note is echoed into its room as a plain-text notice
prefixed with 🎙️, carrying exactly the text the voice message contributed to
the agent's prompt: the transcript, plus any caption. The notice is queued in
the reply outbox, so it is durable, retried and delivered once, ahead of the
agent's own answer. It carries no formatted body, so the transcript is shown
verbatim. A failed transcription posts only the failure notice.

## Limits, retention and recovery

- 20 MiB per input/output file; 100 MiB combined input/output per turn.
- At most five outgoing files per turn.
- 1 GiB per agent across its managed member media and private snapshots. Duplicate
  staging/snapshot copies count. The shared model cache is separate (roughly 631 MiB
  persisted plus the image copy); bounded in-memory audio and `/tmp` WAV are separate.
- Enforce quota before downloads/snapshots and actual limits while streaming. When
  full, reject new work; never evict pinned files. This is a bridge admission limit,
  not an OS disk quota on what an agent can write with its own tools.
- Lower Matrix upload/download limits still apply; failures use the bounded outbox.
- Retain files until seven days after terminal completion, and longer while an
  attachment outbox item needs them. Queued/active/held turns do not expire.
- Run cleanup from the lifecycle worker, not the message/control scheduler. Stop
  and Status remain available while a download/transcription is in progress. Stop
  cancels unsubmitted media turns; an already running preprocessing operation may
  finish within its bounds, but must not then submit a cancelled Hermes turn.
- GC is limited to generated turn directories; do not delete unrelated member data.
  Old selected-conversation history may refer to expired files. Downloading in
  Element is subject to the homeserver's separate media-retention policy.

State version 4 upgrades versions 1–3 without replacing rooms or session IDs.
Back up the stopped bridge's entire `data/` **and the member media folders** for a
consistent recovery. Older controllers refuse the new state; rollback needs the
matching controller/state/media backup, not a hand-edited version number.

## Verification

Local fake-endpoint tests cover durable receipt/dedup, captions, encrypted refusal,
local-media URL checks, checksums/restarts, byte/quota limits, linked/unsafe paths,
output snapshots, persisted upload URIs/stable transaction IDs, held/outbox
retention, managed-credential refusal and Stop during a blocked download. A fake
Hermes run verifies voice-text admission and file-only output through the same
existing run API. Ordinary tests skip the real native-speech smoke.

The explicitly enabled `TestVoiceRuntime` runs in the built runtime image with a
public model sample in WAV and Ogg/Opus formats, no network, no Docker socket and
scratch state. It also checks that an already persisted transcript needs no native
executable on recovery. See
`verification.md` for this change's results and exact artifacts.

To reproduce the native smoke after explicitly authorizing it, build the image and
compile `TestVoiceRuntime` with the normal Go wrapper:

```bash
./hermes-fleet-bridge-matrix-go.sh test -c -o tests/logs/matrix-media.test .
mkdir -p tests/logs/media-scratch
docker run --rm --network none --read-only --cap-drop ALL \
  --security-opt no-new-privileges --memory 3g --cpus 2 \
  --tmpfs /tmp:rw,nosuid,nodev,size=64m,mode=1777 \
  -e TMPDIR=/checks -e HFB_SPEECH_SMOKE=1 \
  -v "$PWD/tests/logs/media-scratch:/checks" \
  -v "$MODEL_TEST_WAVS:/samples:ro" -v "$PWD/tests/logs:/tests:ro" \
  --entrypoint /tests/matrix-media.test hermes-fleet-bridge-matrix:media-check \
  -test.run '^TestVoiceRuntime$' -test.v
```

`MODEL_TEST_WAVS` must be the extracted `test_wavs/` directory from the pinned
Parakeet release (including `0.wav`), not real user audio. Scratch paths must be
writable by the image's UID 1000, or run with the appropriate `--user` override.
The separate real Matrix/Hermes fixture accepts `--image` so validation does not
replace a live image tag:

```bash
python3 tests/integration.py --image hermes-fleet-bridge-matrix:media-check
```
