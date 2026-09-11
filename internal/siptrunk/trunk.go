package siptrunk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wacalls/internal/voip/call"
	"wacalls/internal/voip/core"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// Dialer is what the trunk needs from the owning session to place an
// outgoing WhatsApp call. onCreated runs before the call is started so the
// trunk can register its leg and not miss early state changes.
type Dialer interface {
	DialWhatsApp(ctx context.Context, phone string, onCreated func(callID string, cm *call.CallManager)) (string, error)
}

// Trunk is one SIP trunk bound to one WaCalls session.
type Trunk struct {
	cfg TrunkConfig
	log *slog.Logger
	ua  *sipgo.UserAgent
	dg  *diago.Diago

	dialer     Dialer
	registered atomic.Bool

	mu   sync.Mutex
	legs map[string]*leg
}

// New builds the SIP user agent for a trunk. Call Start to serve.
func New(cfg TrunkConfig, log *slog.Logger) (*Trunk, error) {
	if log == nil {
		log = slog.Default()
	}
	log = log.With("trunk", cfg.Session)

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(cfg.Username),
		sipgo.WithUserAgentHostname(cfg.LocalIP),
	)
	if err != nil {
		return nil, err
	}
	dg := diago.NewDiago(ua,
		diago.WithLogger(log),
		diago.WithTransport(diago.Transport{
			Transport: "udp",
			BindHost:  cfg.LocalIP,
			BindPort:  cfg.LocalPort,
		}),
		diago.WithMediaConfig(diago.MediaConfig{Codecs: codecList(cfg.Codecs)}),
	)
	return &Trunk{cfg: cfg, log: log, ua: ua, dg: dg, legs: map[string]*leg{}}, nil
}

// SetRTPPortRange restricts the UDP ports used for RTP (process-wide).
func SetRTPPortRange(start, end int) {
	if start > 0 && end > start {
		media.RTPPortStart = start
		media.RTPPortEnd = end
	}
}

func codecList(spec string) []media.Codec {
	var out []media.Codec
	for _, name := range strings.Split(spec, ",") {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "alaw", "pcma":
			out = append(out, media.CodecAudioAlaw)
		case "ulaw", "pcmu":
			out = append(out, media.CodecAudioUlaw)
		}
	}
	if len(out) == 0 {
		out = []media.Codec{media.CodecAudioAlaw, media.CodecAudioUlaw}
	}
	return out
}

// Start listens for SIP on the local address and, when configured,
// registers on the PBX. ctx cancellation stops everything.
func (t *Trunk) Start(ctx context.Context, dialer Dialer) error {
	t.dialer = dialer
	if err := t.dg.ServeBackground(ctx, t.serveInvite); err != nil {
		return fmt.Errorf("sip listen %s:%d: %w", t.cfg.LocalIP, t.cfg.LocalPort, err)
	}
	t.log.Info("sip trunk listening", "addr", fmt.Sprintf("%s:%d", t.cfg.LocalIP, t.cfg.LocalPort),
		"pbx", fmt.Sprintf("%s:%d", t.cfg.PBXHost, t.cfg.PBXPort), "register", t.cfg.Register, "did", t.cfg.DID)
	if t.cfg.Register {
		go t.registerLoop(ctx)
	} else {
		go func() {
			<-ctx.Done()
			_ = t.ua.Close()
		}()
	}
	return nil
}

// register runs one registration cycle. diago's deferred Unregister can
// panic when the context is cancelled mid-request (seen on shutdown), so
// the cycle is guarded by recover.
func (t *Trunk) register(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("registration panicked: %v", r)
		}
	}()
	return t.dg.Register(ctx, t.pbxURI(t.cfg.Username), diago.RegisterOptions{
		Username: t.cfg.Username,
		Password: t.cfg.Password,
		Expiry:   120 * time.Second,
		OnRegistered: func() {
			t.registered.Store(true)
			t.log.Info("registered on pbx", "user", t.cfg.Username)
		},
	})
}

// Registered reports whether the trunk currently holds a registration on the PBX.
func (t *Trunk) Registered() bool { return t.registered.Load() }

func (t *Trunk) pbxURI(user string) sip.Uri {
	return sip.Uri{
		Scheme:    "sip",
		User:      user,
		Host:      t.cfg.PBXHost,
		Port:      t.cfg.PBXPort,
		UriParams: sip.NewParams(),
	}
}

func (t *Trunk) registerLoop(ctx context.Context) {
	defer t.ua.Close()
	for {
		err := t.register(ctx)
		t.registered.Store(false)
		if ctx.Err() != nil {
			return
		}
		t.log.Warn("pbx registration ended, retrying", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

// ---- leg registry -------------------------------------------------------

func (t *Trunk) addLeg(lg *leg) {
	t.mu.Lock()
	t.legs[lg.callID] = lg
	t.mu.Unlock()
}

func (t *Trunk) removeLeg(callID string) {
	t.mu.Lock()
	lg, ok := t.legs[callID]
	delete(t.legs, callID)
	t.mu.Unlock()
	if ok {
		lg.close()
	}
}

func (t *Trunk) getLeg(callID string) (*leg, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	lg, ok := t.legs[callID]
	return lg, ok
}

func (t *Trunk) activeCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.legs)
}

// ---- hooks called by the session -----------------------------------------

// OnCallState forwards a WhatsApp call state change to its leg. Safe to call
// under the CallManager lock.
func (t *Trunk) OnCallState(callID string, info *call.CallInfo) {
	if lg, ok := t.getLeg(callID); ok {
		lg.onState(info)
	}
}

// OnCallEnded forwards a WhatsApp-side hangup to its leg.
func (t *Trunk) OnCallEnded(callID string, info *call.CallInfo) {
	if lg, ok := t.getLeg(callID); ok {
		lg.markEnded(info.StateData.EndReason)
	}
}

// OnPeerAudio forwards decoded WhatsApp audio (16 kHz float32) to its leg.
func (t *Trunk) OnPeerAudio(callID string, pcm []float32) {
	if lg, ok := t.getLeg(callID); ok {
		lg.onPeerAudio(pcm)
	}
}

// OnIncoming handles a ringing WhatsApp call: it is offered to the PBX as a
// SIP INVITE and accepted on WhatsApp only once the PBX answers.
func (t *Trunk) OnIncoming(cm *call.CallManager, info *call.CallInfo, callerPhone string) {
	if callerPhone == "" {
		callerPhone = "anonymous"
	}
	if t.activeCount() >= t.cfg.MaxCalls {
		t.log.Warn("incoming whatsapp call rejected: trunk busy", "call_id", info.CallID, "caller", callerPhone)
		_ = cm.RejectCall(context.Background(), info.CallID, core.EndCallReasonBusy)
		return
	}
	lg := newLeg(info.CallID, "inbound", callerPhone, cm, t.log)
	t.addLeg(lg)
	go t.runInbound(lg)
}

func (t *Trunk) runInbound(lg *leg) {
	defer t.removeLeg(lg.callID)
	bg := context.Background()
	ringCtx, cancelRing := context.WithTimeout(bg, time.Duration(t.cfg.RingTimeoutSec)*time.Second)
	defer cancelRing()

	d, err := t.dg.NewDialog(t.pbxURI(t.cfg.DID), diago.NewDialogOptions{})
	if err != nil {
		lg.log.Error("pbx dialog create failed", "err", err)
		_ = lg.cm.RejectCall(bg, lg.callID, core.EndCallReasonFailed)
		return
	}
	defer d.Close()

	// WhatsApp caller gave up while the PBX is still ringing → CANCEL.
	go func() {
		select {
		case <-lg.endedCh:
			cancelRing()
		case <-lg.done:
		}
	}()

	opts := diago.InviteClientOptions{
		Username: t.cfg.Username,
		Password: t.cfg.Password,
		OnResponse: func(res *sip.Response) error {
			if res.IsProvisional() {
				lg.log.Debug("pbx provisional", "status", res.StatusCode)
			}
			return nil
		},
	}
	opts.WithCaller(t.cfg.CallerName, lg.peer, t.cfg.LocalIP)

	lg.log.Info("offering whatsapp call to pbx", "did", t.cfg.DID)
	if err := d.Invite(ringCtx, opts); err != nil {
		if lg.ended() {
			lg.log.Info("whatsapp caller hung up before pbx answered")
			return
		}
		reason := core.EndCallReasonDeclined
		var dr sipgo.ErrDialogResponse
		if errors.As(err, &dr) && dr.Res != nil {
			switch dr.Res.StatusCode {
			case sip.StatusBusyHere:
				reason = core.EndCallReasonBusy
			case sip.StatusTemporarilyUnavailable, sip.StatusRequestTimeout:
				reason = core.EndCallReasonTimeout
			}
			lg.log.Warn("pbx did not answer", "status", dr.Res.StatusCode, "reason", dr.Res.Reason)
		} else if errors.Is(err, context.DeadlineExceeded) {
			reason = core.EndCallReasonTimeout
			lg.log.Warn("pbx ring timeout")
		} else {
			lg.log.Error("pbx invite failed", "err", err)
			reason = core.EndCallReasonFailed
		}
		_ = lg.cm.RejectCall(bg, lg.callID, reason)
		return
	}
	if err := d.Ack(bg); err != nil {
		lg.log.Error("pbx ack failed", "err", err)
		_ = lg.cm.RejectCall(bg, lg.callID, core.EndCallReasonFailed)
		return
	}
	if lg.ended() {
		_ = d.Hangup(bg)
		return
	}
	if err := lg.cm.AcceptCall(bg, lg.callID); err != nil {
		lg.log.Error("whatsapp accept failed", "err", err)
		_ = d.Hangup(bg)
		return
	}
	if err := lg.startMedia(&d.DialogMedia); err != nil {
		lg.log.Error("media setup failed", "err", err)
		_ = lg.cm.EndCall(bg, core.EndCallReasonFailed)
		_ = d.Hangup(bg)
		return
	}
	lg.log.Info("call bridged (whatsapp → pbx)")

	select {
	case <-d.Context().Done():
		lg.log.Info("pbx hung up")
		_ = lg.cm.EndCall(bg, core.EndCallReasonUserEnded)
	case <-lg.endedCh:
		lg.log.Info("whatsapp hung up", "reason", lg.reason())
		_ = d.Hangup(bg)
	}
}

// serveInvite handles an INVITE from the PBX: dial the To user on WhatsApp.
func (t *Trunk) serveInvite(in *diago.DialogServerSession) {
	log := t.log.With("sip_call", in.ID)
	_ = in.Trying()

	number := digits(in.ToUser())
	if len(number) < 8 {
		log.Warn("invite rejected: bad number", "to", in.ToUser())
		_ = in.Respond(sip.StatusNotFound, "Not Found", nil)
		return
	}
	if t.dialer == nil || t.activeCount() >= t.cfg.MaxCalls {
		log.Warn("invite rejected: trunk busy", "number", number)
		_ = in.Respond(sip.StatusBusyHere, "Busy Here", nil)
		return
	}

	var lg *leg
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 20*time.Second)
	callID, err := t.dialer.DialWhatsApp(dialCtx, number, func(callID string, cm *call.CallManager) {
		lg = newLeg(callID, "outbound", number, cm, t.log)
		t.addLeg(lg)
	})
	cancelDial()
	if err != nil {
		if lg != nil {
			t.removeLeg(lg.callID)
		}
		log.Error("whatsapp dial failed", "number", number, "err", err)
		_ = in.Respond(sip.StatusServiceUnavailable, "Service Unavailable", nil)
		return
	}
	defer t.removeLeg(callID)
	lg.log.Info("dialing whatsapp for pbx", "from", in.FromUser())

	bg := context.Background()
	sipCtx := in.Context()
	ringTimer := time.NewTimer(time.Duration(t.cfg.RingTimeoutSec) * time.Second)
	defer ringTimer.Stop()

	ringingCh, activeCh := lg.ringingCh, lg.activeCh
	answered := false
	for {
		select {
		case <-sipCtx.Done():
			// CANCEL before answer, BYE after.
			reason := core.EndCallReasonCancelled
			if answered {
				reason = core.EndCallReasonUserEnded
			}
			lg.log.Info("pbx ended call", "answered", answered)
			_ = lg.cm.EndCall(bg, reason)
			return

		case <-ringingCh:
			ringingCh = nil
			if !answered {
				_ = in.Ringing()
			}

		case <-activeCh:
			activeCh = nil
			if answered {
				continue
			}
			if err := in.Answer(); err != nil {
				lg.log.Error("pbx answer failed", "err", err)
				_ = lg.cm.EndCall(bg, core.EndCallReasonFailed)
				return
			}
			answered = true
			if err := lg.startMedia(&in.DialogMedia); err != nil {
				lg.log.Error("media setup failed", "err", err)
				_ = lg.cm.EndCall(bg, core.EndCallReasonFailed)
				_ = in.Hangup(bg)
				return
			}
			lg.log.Info("call bridged (pbx → whatsapp)")

		case <-lg.endedCh:
			reason := lg.reason()
			if answered {
				lg.log.Info("whatsapp hung up", "reason", reason)
				_ = in.Hangup(bg)
				return
			}
			code, text := sipStatusForReason(reason)
			lg.log.Info("whatsapp call not answered", "reason", reason, "sip_status", code)
			_ = in.Respond(code, text, nil)
			return

		case <-ringTimer.C:
			if answered {
				continue
			}
			lg.log.Warn("whatsapp ring timeout")
			_ = lg.cm.EndCall(bg, core.EndCallReasonTimeout)
			_ = in.Respond(sip.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil)
			return
		}
	}
}

// sipStatusForReason maps a WhatsApp end reason to the SIP final response
// the PBX gets when an outbound call fails before being answered.
func sipStatusForReason(r core.EndCallReason) (int, string) {
	switch r {
	case core.EndCallReasonDeclined:
		return 603, "Decline"
	case core.EndCallReasonBusy:
		return sip.StatusBusyHere, "Busy Here"
	case core.EndCallReasonTimeout, core.EndCallReasonDoNotDisturb:
		return sip.StatusTemporarilyUnavailable, "Temporarily Unavailable"
	case core.EndCallReasonCancelled:
		return sip.StatusRequestTerminated, "Request Terminated"
	default:
		// failed / unknown: let the PBX fail over to the next trunk.
		return sip.StatusServiceUnavailable, "Service Unavailable"
	}
}

func digits(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}
