package sarvam

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

// TTS is a live synthesis socket.
//
// One connection serves a whole call: config goes once, then text is streamed in and audio comes
// back as it is produced. Reconnecting per utterance would add a handshake to every single turn.
type TTS struct {
	conn  *websocket.Conn
	audio chan []byte

	mu     sync.Mutex
	failed string
}

// Err reports the provider error that closed the socket, if one did.
//
// Without this a rejected config — a speaker the chosen model does not serve, say — surfaces much
// later and as something else entirely: the socket closes quietly, and the first write after it
// fails with "use of closed network connection", which points nowhere near the real cause.
func (t *TTS) Err() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.failed
}

// ttsConfig is the one-time setup message.
type ttsConfig struct {
	Type string `json:"type"`
	Data struct {
		Speaker          string  `json:"speaker"`
		LanguageCode     string  `json:"language_code"`
		Pace             float64 `json:"pace"`
		MinBufferSize    int     `json:"min_buffer_size"`
		MaxChunkLength   int     `json:"max_chunk_length"`
		OutputAudioCodec string  `json:"output_audio_codec"`
		SampleRate       int     `json:"sample_rate"`
	} `json:"data"`
}

// ttsText streams a fragment to be spoken.
type ttsText struct {
	Type string `json:"type"`
	Data struct {
		Text string `json:"text"`
	} `json:"data"`
}

// ttsFlush forces out whatever is buffered, used at the end of a turn.
type ttsFlush struct {
	Type string `json:"type"`
}

// ttsMessage is anything coming back.
type ttsMessage struct {
	Type string `json:"type"`
	Data struct {
		Audio   string `json:"audio"`
		Message string `json:"message"`
	} `json:"data"`
	Audio string `json:"audio"`
}

// DialTTS opens a synthesis socket for one call.
//
// linear16 is requested rather than a compressed codec: the call carries raw PCM, so anything else
// would mean decoding on the phone for no benefit. min_buffer_size is kept small because
// time-to-first-audio matters far more here than throughput — a long buffer is a long silence.
func DialTTS(ctx context.Context, cfg Config, language, speaker string, pace float64) (*TTS, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("sarvam: api key is required")
	}
	model := cfg.TTSModel
	if model == "" {
		model = DefaultTTSModel
	}
	endpoint := cfg.TTSURL
	if endpoint == "" {
		endpoint = "wss://api.sarvam.ai/text-to-speech/ws"
	}
	if speaker == "" {
		speaker = DefaultSpeaker
	}
	if pace <= 0 {
		// Slightly slower than default: the listener is often elderly and may be hard of hearing.
		pace = 0.95
	}

	q := url.Values{}
	q.Set("model", model)

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, endpoint+"?"+q.Encode(), &websocket.DialOptions{
		HTTPHeader: http.Header{"api-subscription-key": []string{cfg.APIKey}},
	})
	if err != nil {
		return nil, fmt.Errorf("sarvam tts dial: %w", err)
	}
	conn.SetReadLimit(readLimit)

	var conf ttsConfig
	conf.Type = "config"
	conf.Data.Speaker = speaker
	conf.Data.LanguageCode = language
	conf.Data.Pace = pace
	conf.Data.MinBufferSize = 30
	conf.Data.MaxChunkLength = 200
	conf.Data.OutputAudioCodec = "linear16"
	conf.Data.SampleRate = SampleRate

	msg, err := json.Marshal(conf)
	if err != nil {
		conn.CloseNow()
		return nil, err
	}
	if err := conn.Write(dialCtx, websocket.MessageText, msg); err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("sarvam tts config: %w", err)
	}

	t := &TTS{conn: conn, audio: make(chan []byte, 64)}
	go t.read(ctx)
	return t, nil
}

// Speak streams a fragment of text.
//
// Callers should send each sentence as the model produces it rather than waiting for a whole
// response; synthesis of the first clause then overlaps generation of the rest.
func (t *TTS) Speak(ctx context.Context, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var m ttsText
	m.Type = "text"
	m.Data.Text = text

	msg, err := json.Marshal(m)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return t.conn.Write(wctx, websocket.MessageText, msg)
}

// Flush forces buffered text to be synthesised, ending a turn.
func (t *TTS) Flush(ctx context.Context) error {
	msg, err := json.Marshal(ttsFlush{Type: "flush"})
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return t.conn.Write(wctx, websocket.MessageText, msg)
}

// Audio yields synthesised s16le PCM until the socket closes.
func (t *TTS) Audio() <-chan []byte { return t.audio }

// read pumps synthesised audio into the channel.
func (t *TTS) read(ctx context.Context) {
	defer close(t.audio)
	for {
		_, data, err := t.conn.Read(ctx)
		if err != nil {
			return
		}
		var m ttsMessage
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		if m.Type == "error" {
			t.mu.Lock()
			t.failed = m.Data.Message
			t.mu.Unlock()
			return
		}
		encoded := m.Data.Audio
		if encoded == "" {
			encoded = m.Audio
		}
		if encoded == "" {
			continue
		}
		pcm, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(pcm) == 0 {
			continue
		}
		select {
		case t.audio <- pcm:
		case <-ctx.Done():
			return
		}
	}
}

// Close shuts the socket down.
func (t *TTS) Close() error {
	return t.conn.Close(websocket.StatusNormalClosure, "")
}

// SplitSentences breaks generated text at clause boundaries so synthesis can start on the first
// fragment while the rest is still being generated.
//
// It deliberately also breaks on Devanagari danda and the Arabic-script full stop, since the
// languages this serves are not all punctuated with ASCII.
func SplitSentences(s string) []string {
	var out []string
	var b strings.Builder
	for _, r := range s {
		b.WriteRune(r)
		switch r {
		case '.', '!', '?', '\n', '।', '॥', '۔':
			if frag := strings.TrimSpace(b.String()); frag != "" {
				out = append(out, frag)
			}
			b.Reset()
		}
	}
	if frag := strings.TrimSpace(b.String()); frag != "" {
		out = append(out, frag)
	}
	return out
}
