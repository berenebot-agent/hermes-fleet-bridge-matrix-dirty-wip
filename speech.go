package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const voiceSeconds = 300
const pcmRate = 16000 * 2

var speechSlot = make(chan struct{}, 1)
var speechFiles = map[string]string{
	"encoder.int8.onnx": "a32b12d17bbbc309d0686fbbcc2987b5e9b8333a7da83fa6b089f0a2acd651ab",
	"decoder.int8.onnx": "b6bb64963457237b900e496ee9994b59294526439fbcc1fecf705b31a15c6b4e",
	"joiner.int8.onnx":  "7946164367946e7f9f29a122407c3252b680dbae9a51343eb2488d057c3c43d2",
	"tokens.txt":        "ec182b70dd42113aff6c5372c75cac58c952443eb22322f57bbd7f53977d497d",
}

// Native speech tools inherit no Matrix/Hermes credentials. Never surface their output on error.
func speechCommand(ctx context.Context, input io.Reader, limit int, program string, args ...string) ([]byte, error) {
	command := append([]string{"--as=2147483648", "--cpu=120", "--", program}, args...)
	cmd := exec.CommandContext(ctx, "prlimit", command...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/tmp", "OMP_NUM_THREADS=2", "OPENBLAS_NUM_THREADS=1"}
	cmd.Stdin = input
	stdout, stderr := &cappedBuffer{limit: limit}, &cappedBuffer{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second
	if cmd.Run() != nil {
		return nil, errors.New("Local voice processing failed or exceeded its resource/time limit.")
	}
	return stdout.Bytes(), nil
}

func modelDigest(f *os.File) (string, error) {
	h := sha256.New()
	_, err := io.Copy(h, io.LimitReader(f, 700<<20))
	return hex.EncodeToString(h.Sum(nil)), err
}

func speechModel(c Config) (string, error) {
	r, err := os.OpenRoot(filepath.Join(c.Data, "controller"))
	if err != nil {
		return "", err
	}
	defer r.Close()
	models, err := mediaDir(r, "models", true)
	if err != nil {
		return "", err
	}
	defer models.Close()
	model, err := mediaDir(models, "parakeet-v2-int8", true)
	if err != nil {
		return "", err
	}
	defer model.Close()
	for name, digest := range speechFiles {
		f, err := regularMedia(model, name)
		if err != nil {
			if _, statErr := model.Lstat(name); !os.IsNotExist(statErr) {
				return "", errors.New("Unsafe speech model file; restore the pinned model cache.")
			}
			source, err := os.Open(filepath.Join("/opt/parakeet", name))
			if err != nil {
				return "", err
			}
			actual, _, err := writeMedia(model, name, source, 700<<20)
			source.Close()
			if err != nil || actual != digest {
				return "", errors.New("Bundled speech model checksum mismatch.")
			}
		} else {
			actual, err := modelDigest(f)
			f.Close()
			if err != nil || actual != digest {
				return "", errors.New("Speech model checksum mismatch; restore the pinned model cache.")
			}
		}
	}
	return filepath.Join(c.Data, "controller", "models", "parakeet-v2-int8"), nil
}

func pcmWave(pcm []byte) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint32(16000))
	binary.Write(&b, binary.LittleEndian, uint32(pcmRate))
	binary.Write(&b, binary.LittleEndian, uint16(2))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

func speechTranscript(output []byte) (string, error) {
	var result struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(output, &result) != nil || strings.TrimSpace(result.Text) == "" || len(result.Text) > 16<<10 {
		return "", errors.New("Voice note produced no usable transcript (silence, unsupported audio, or text over 16 KiB).")
	}
	return strings.TrimSpace(result.Text), nil
}

func validateVoicePCM(pcm []byte) error {
	if len(pcm) > voiceSeconds*pcmRate {
		return errors.New("Voice note exceeds the five-minute limit.")
	}
	if len(pcm) < 3200 || len(pcm)%2 != 0 {
		return errors.New("Voice note contains no usable audio.")
	}
	for _, b := range pcm {
		if b != 0 {
			return nil
		}
	}
	return errors.New("Voice note contains only digital silence.")
}

func transcribeVoice(ctx context.Context, c Config, m *Member, t *Turn) (string, error) {
	select {
	case speechSlot <- struct{}{}:
		defer func() { <-speechSlot }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	r, err := memberMedia(m)
	if err != nil {
		return "", errors.New("Voice media storage unavailable.")
	}
	defer r.Close()
	dir, err := mediaDir(r, "incoming", false)
	if err != nil {
		return "", errors.New("Voice media storage unavailable.")
	}
	defer dir.Close()
	turn, err := mediaDir(dir, stableID(t.ID), false)
	if err != nil {
		return "", errors.New("Voice recording unavailable.")
	}
	defer turn.Close()
	f, err := regularMedia(turn, "file")
	if err != nil {
		return "", errors.New("Voice recording is unsafe or missing.")
	}
	// Decode an immutable in-memory copy: the agent can write its own incoming directory.
	data, err := io.ReadAll(io.LimitReader(f, maxAttachment+1))
	f.Close()
	digest := sha256.Sum256(data)
	if err != nil || len(data) > maxAttachment || int64(len(data)) != t.Media.Size || hex.EncodeToString(digest[:]) != t.Media.SHA256 {
		return "", errors.New("Voice recording changed; resend it explicitly.")
	}
	pcm, err := speechCommand(ctx, bytes.NewReader(data), (voiceSeconds+1)*pcmRate, "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-threads", "1", "-protocol_whitelist", "pipe", "-i", "pipe:0", "-map", "0:a:0", "-vn", "-sn", "-dn", "-ac", "1", "-ar", "16000", "-t", "301", "-threads", "1", "-f", "s16le", "pipe:1")
	if err != nil {
		return "", err
	}
	if err := validateVoicePCM(pcm); err != nil {
		return "", err
	}
	model, err := speechModel(c)
	if err != nil {
		return "", errors.New("Pinned local speech model unavailable or invalid; inspect the private model cache.")
	}
	wav, err := os.CreateTemp("/tmp", "hfb-voice-*.wav")
	if err != nil {
		return "", errors.New("Cannot prepare temporary voice audio.")
	}
	defer os.Remove(wav.Name())
	_, err = wav.Write(pcmWave(pcm))
	closeErr := wav.Close()
	if err != nil || closeErr != nil {
		return "", errors.New("Cannot write temporary voice audio.")
	}
	output, err := speechCommand(ctx, nil, 1<<20, "sherpa-onnx-offline", "--tokens="+filepath.Join(model, "tokens.txt"), "--encoder="+filepath.Join(model, "encoder.int8.onnx"), "--decoder="+filepath.Join(model, "decoder.int8.onnx"), "--joiner="+filepath.Join(model, "joiner.int8.onnx"), "--model-type=nemo_transducer", "--num-threads=2", "--print-args=false", wav.Name())
	if err != nil {
		return "", err
	}
	return speechTranscript(output)
}
