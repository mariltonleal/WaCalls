// Package siptrunk turns a WaCalls session into a SIP trunk for a PBX
// (Asterisk / FreePBX / Issabel). WhatsApp voice calls become SIP dialogs
// carrying G.711 audio, in both directions:
//
//   - WhatsApp → PBX: an incoming WhatsApp call sends an INVITE to the PBX with
//     the caller's phone number as caller-ID and the configured DID as the
//     dialed number. The WhatsApp call is only accepted once the PBX answers.
//   - PBX → WhatsApp: an INVITE from the PBX (To user = destination number)
//     starts a WhatsApp call. 180 Ringing while it rings, 200 OK when the
//     peer picks up, and a mapped 4xx/5xx/6xx when it fails.
//
// The call core only ever sees 16 kHz float32 PCM, so this package resamples
// between 16 kHz (MLow) and 8 kHz (G.711).
package siptrunk

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// TrunkConfig describes one SIP trunk bound to one WaCalls session.
type TrunkConfig struct {
	// Session is the WaCalls session name (case-insensitive) or session id
	// this trunk is attached to.
	Session string `json:"session"`

	// LocalIP / LocalPort is where this trunk listens for SIP (UDP) and
	// where its Contact points. Each trunk needs its own port.
	LocalIP   string `json:"local_ip"`
	LocalPort int    `json:"local_port"`

	// PBXHost / PBXPort is the SIP address of the PBX (Asterisk).
	PBXHost string `json:"pbx_host"`
	PBXPort int    `json:"pbx_port"`

	// Username / Password are the digest credentials used both to REGISTER
	// on the PBX (when Register is true) and to answer 401 challenges on
	// INVITEs we send to the PBX.
	Username string `json:"username"`
	Password string `json:"password"`

	// Register makes the trunk REGISTER on the PBX. When false the PBX must
	// identify the trunk by IP/port (static trunk).
	Register bool `json:"register"`

	// DID is the number presented as the dialed number (Request-URI / To
	// user) when a WhatsApp call is delivered to the PBX. The PBX inbound
	// route matches on it.
	DID string `json:"did"`

	// CallerName is the display name put in the From header of calls
	// delivered to the PBX.
	CallerName string `json:"caller_name"`

	// Codecs is the preferred codec list offered to the PBX: "alaw", "ulaw".
	// Default: alaw,ulaw.
	Codecs string `json:"codecs"`

	// RingTimeoutSec bounds how long a call may ring in either direction.
	RingTimeoutSec int `json:"ring_timeout_sec"`

	// MaxCalls caps concurrent calls on this trunk (WhatsApp normally allows
	// one voice call per linked device). Default 1.
	MaxCalls int `json:"max_calls"`
}

// Config is the on-disk trunk configuration (JSON).
type Config struct {
	// RTPPortStart / RTPPortEnd restrict the UDP port range used for RTP
	// towards the PBX. 0 means ephemeral ports.
	RTPPortStart int `json:"rtp_port_start"`
	RTPPortEnd   int `json:"rtp_port_end"`

	Trunks []TrunkConfig `json:"trunks"`
}

// Load reads and validates a trunk configuration file. An empty path yields
// an empty configuration (no trunks) and no error.
func Load(path string) (*Config, error) {
	cfg := &Config{}
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) applyDefaults() error {
	if (c.RTPPortStart == 0) != (c.RTPPortEnd == 0) {
		return fmt.Errorf("rtp_port_start and rtp_port_end must be set together")
	}
	if c.RTPPortEnd != 0 && c.RTPPortEnd <= c.RTPPortStart {
		return fmt.Errorf("rtp_port_end must be greater than rtp_port_start")
	}
	seenPorts := map[string]string{}
	seenSessions := map[string]bool{}
	for i := range c.Trunks {
		t := &c.Trunks[i]
		t.Session = strings.TrimSpace(t.Session)
		if t.Session == "" {
			return fmt.Errorf("trunk %d: session is required", i)
		}
		key := strings.ToLower(t.Session)
		if seenSessions[key] {
			return fmt.Errorf("trunk %d: session %q used twice", i, t.Session)
		}
		seenSessions[key] = true
		if t.LocalIP == "" {
			t.LocalIP = "127.0.0.1"
		}
		if t.LocalPort == 0 {
			return fmt.Errorf("trunk %q: local_port is required", t.Session)
		}
		addr := fmt.Sprintf("%s:%d", t.LocalIP, t.LocalPort)
		if other, dup := seenPorts[addr]; dup {
			return fmt.Errorf("trunk %q: %s already used by trunk %q", t.Session, addr, other)
		}
		seenPorts[addr] = t.Session
		if t.PBXHost == "" {
			t.PBXHost = "127.0.0.1"
		}
		if t.PBXPort == 0 {
			t.PBXPort = 5060
		}
		if t.Register && (t.Username == "" || t.Password == "") {
			return fmt.Errorf("trunk %q: username and password are required when register=true", t.Session)
		}
		if t.Username == "" {
			t.Username = "wacalls"
		}
		if t.DID == "" {
			return fmt.Errorf("trunk %q: did is required", t.Session)
		}
		if t.CallerName == "" {
			t.CallerName = "WhatsApp"
		}
		if t.Codecs == "" {
			t.Codecs = "alaw,ulaw"
		}
		for _, name := range strings.Split(t.Codecs, ",") {
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "alaw", "pcma", "ulaw", "pcmu":
			default:
				return fmt.Errorf("trunk %q: unsupported codec %q (use alaw/ulaw)", t.Session, name)
			}
		}
		if t.RingTimeoutSec <= 0 {
			t.RingTimeoutSec = 60
		}
		if t.MaxCalls <= 0 {
			t.MaxCalls = 1
		}
	}
	return nil
}

// Find returns the trunk configured for a session, matching by name
// (case-insensitive) or by id. Nil when the session has no trunk.
func (c *Config) Find(sessionName, sessionID string) *TrunkConfig {
	if c == nil {
		return nil
	}
	for i := range c.Trunks {
		t := &c.Trunks[i]
		if strings.EqualFold(t.Session, sessionName) || t.Session == sessionID {
			return t
		}
	}
	return nil
}
