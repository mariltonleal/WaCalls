package siptrunk

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"

	"wacalls/internal/voip/call"
	"wacalls/internal/voip/core"

	"github.com/emiago/diago"
	"github.com/emiago/diago/audio"
	"github.com/emiago/diago/media"
)

// leg is one bridged call: a WhatsApp CallManager on one side and a SIP
// dialog (DialogMedia) on the other. It owns the audio pumps.
type leg struct {
	callID    string
	direction string // "inbound" (WhatsApp → PBX) or "outbound" (PBX → WhatsApp)
	peer      string // phone number on the WhatsApp side
	cm        *call.CallManager
	log       *slog.Logger

	// State signals from the WhatsApp side. Each channel is closed once.
	ringingCh chan struct{}
	activeCh  chan struct{}
	endedCh   chan struct{}
	ringOnce  sync.Once
	actOnce   sync.Once
	endOnce   sync.Once
	endReason atomic.Value // core.EndCallReason

	// done is closed when the leg is torn down (either side hung up).
	done     chan struct{}
	doneOnce sync.Once

	// Audio towards the PBX: fixed-size 8 kHz LPCM chunks (one RTP packet each).
	toSIP      chan []byte
	chunkBytes atomic.Int32
	mediaReady atomic.Bool

	down    *Downsampler
	up      *Upsampler
	pendMu  sync.Mutex
	pending []int16
}

func newLeg(callID, direction, peer string, cm *call.CallManager, log *slog.Logger) *leg {
	lg := &leg{
		callID:    callID,
		direction: direction,
		peer:      peer,
		cm:        cm,
		log:       log.With("call_id", callID, "direction", direction, "peer", peer),
		ringingCh: make(chan struct{}),
		activeCh:  make(chan struct{}),
		endedCh:   make(chan struct{}),
		done:      make(chan struct{}),
		toSIP:     make(chan []byte, 25), // ~500 ms of audio
		down:      NewDownsampler(),
		up:        NewUpsampler(),
	}
	lg.chunkBytes.Store(320) // 20 ms @ 8 kHz, 16-bit
	lg.endReason.Store(core.EndCallReasonUnknown)
	return lg
}

// onState receives WhatsApp call state changes. It is called from inside the
// CallManager (possibly under its lock), so it must never call back into it.
func (lg *leg) onState(info *call.CallInfo) {
	switch info.StateData.State {
	case core.CallStateRinging, core.CallStateIncomingRinging:
		lg.ringOnce.Do(func() { close(lg.ringingCh) })
	case core.CallStateActive:
		lg.actOnce.Do(func() { close(lg.activeCh) })
	case core.CallStateEnded:
		lg.markEnded(info.StateData.EndReason)
	}
}

func (lg *leg) markEnded(reason core.EndCallReason) {
	lg.endOnce.Do(func() {
		if reason == "" {
			reason = core.EndCallReasonUnknown
		}
		lg.endReason.Store(reason)
		close(lg.endedCh)
	})
}

func (lg *leg) ended() bool {
	select {
	case <-lg.endedCh:
		return true
	default:
		return false
	}
}

func (lg *leg) reason() core.EndCallReason {
	r, _ := lg.endReason.Load().(core.EndCallReason)
	return r
}

func (lg *leg) close() {
	lg.doneOnce.Do(func() { close(lg.done) })
}

// startMedia wires the negotiated SIP media to the WhatsApp call: RTP in
// → G.711 decode → 8→16 kHz → CallManager, and CallManager → 16→8 kHz →
// G.711 encode → RTP out.
func (lg *leg) startMedia(m *diago.DialogMedia) error {
	props := diago.MediaProps{}
	reader, err := m.AudioReader(diago.WithAudioReaderMediaProps(&props))
	if err != nil {
		return err
	}
	writer, err := m.AudioWriter()
	if err != nil {
		return err
	}
	dec, err := audio.NewPCMDecoderReader(props.Codec.PayloadType, reader)
	if err != nil {
		return err
	}
	enc, err := audio.NewPCMEncoderWriter(props.Codec.PayloadType, writer)
	if err != nil {
		return err
	}
	lg.chunkBytes.Store(int32(props.Codec.Samples16()))
	lg.mediaReady.Store(true)
	lg.log.Info("media bridged", "codec", props.Codec.Name, "rtp_local", props.Laddr, "rtp_remote", props.Raddr)

	go lg.pumpToWhatsApp(dec)
	go lg.pumpToSIP(enc)
	return nil
}

// pumpToSIP writes one 20 ms chunk per tick towards the PBX. The RTP writer
// paces itself on its own clock, so this loop simply keeps it fed — with
// silence whenever WhatsApp has nothing for us (keeps the PBX jitter buffer
// and RTP timeouts happy).
func (lg *leg) pumpToSIP(enc *audio.PCMEncoderWriter) {
	silence := make([]byte, lg.chunkBytes.Load())
	for {
		select {
		case <-lg.done:
			return
		default:
		}
		var chunk []byte
		select {
		case chunk = <-lg.toSIP:
		default:
			chunk = silence
		}
		if _, err := enc.Write(chunk); err != nil {
			if !lg.closed() {
				lg.log.Debug("rtp write ended", "err", err)
			}
			return
		}
	}
}

// pumpToWhatsApp reads decoded 8 kHz PCM from the PBX and feeds it to the
// WhatsApp encoder at 16 kHz.
func (lg *leg) pumpToWhatsApp(dec *audio.PCMDecoderReader) {
	buf := make([]byte, media.RTPBufSize)
	for {
		n, err := dec.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() && !lg.closed() {
				continue
			}
			if !lg.closed() && !errors.Is(err, io.EOF) {
				lg.log.Debug("rtp read ended", "err", err)
			}
			return
		}
		if n == 0 {
			continue
		}
		pcm := lg.up.Process(bytesLEToInt16(buf[:n]))
		lg.cm.FeedCapturedPCM(pcm)
	}
}

// onPeerAudio receives 16 kHz float32 PCM decoded from WhatsApp and queues
// it as fixed-size 8 kHz chunks for the SIP writer. Oldest audio is dropped
// when the PBX side is not consuming (never blocks the call core).
func (lg *leg) onPeerAudio(pcm []float32) {
	if !lg.mediaReady.Load() {
		return
	}
	samplesPerChunk := int(lg.chunkBytes.Load()) / 2
	lg.pendMu.Lock()
	defer lg.pendMu.Unlock()
	lg.pending = append(lg.pending, lg.down.Process(pcm)...)
	for len(lg.pending) >= samplesPerChunk {
		chunk := int16ToBytesLE(lg.pending[:samplesPerChunk])
		lg.pending = lg.pending[samplesPerChunk:]
		select {
		case lg.toSIP <- chunk:
		default:
			select {
			case <-lg.toSIP: // drop oldest
			default:
			}
			select {
			case lg.toSIP <- chunk:
			default:
			}
		}
	}
}

func (lg *leg) closed() bool {
	select {
	case <-lg.done:
		return true
	default:
		return false
	}
}
