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
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
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

	// TrunkCreateHook, when set, is a command run after a trunk is added
	// through the API, with arguments: <name> <password> <did>. Use it to
	// create the matching trunk on the PBX automatically.
	TrunkCreateHook string `json:"trunk_create_hook,omitempty"`

	Trunks []TrunkConfig `json:"trunks"`

	mu   sync.Mutex `json:"-"`
	path string
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
	cfg.path = path
	return cfg, nil
}

// Path returns the file this configuration was loaded from ("" when none).
func (c *Config) Path() string {
	if c == nil {
		return ""
	}
	return c.path
}

// Reload re-reads the configuration file so edits made by hand apply to
// sessions created afterwards. A configuration without a file is a no-op.
func (c *Config) Reload() error {
	if c == nil || c.path == "" {
		return nil
	}
	fresh, err := Load(c.path)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.RTPPortStart, c.RTPPortEnd = fresh.RTPPortStart, fresh.RTPPortEnd
	c.TrunkCreateHook = fresh.TrunkCreateHook
	c.Trunks = fresh.Trunks
	c.mu.Unlock()
	return nil
}

// Save writes the configuration back to its file (mode 0600, atomically).
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("trunk configuration file not set (start with -trunks)")
	}
	c.mu.Lock()
	data, err := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// List returns a copy of the configured trunks.
func (c *Config) List() []TrunkConfig {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]TrunkConfig, len(c.Trunks))
	copy(out, c.Trunks)
	return out
}

var trunkNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{1,31}$`)

// Add creates a trunk for a new session: next free local port, random
// password, username = name (FreePBX names the auth user after the trunk,
// so the PBX trunk must be created with exactly this name). It persists
// the file and returns the resulting entry.
func (c *Config) Add(name, did string) (*TrunkConfig, error) {
	if c == nil || c.path == "" {
		return nil, fmt.Errorf("trunk configuration file not set (start with -trunks)")
	}
	name = strings.TrimSpace(name)
	if !trunkNameRe.MatchString(name) {
		return nil, fmt.Errorf("invalid name %q: use 2-32 letters, digits, - or _", name)
	}
	did = digitsOnly(did)
	if len(did) < 8 || len(did) > 15 {
		return nil, fmt.Errorf("invalid did %q: use the number in E.164 without +, e.g. 5519987654321", did)
	}
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	password := base64.RawURLEncoding.EncodeToString(raw)

	c.mu.Lock()
	port := 5070
	for _, t := range c.Trunks {
		if strings.EqualFold(t.Session, name) {
			c.mu.Unlock()
			return nil, fmt.Errorf("trunk %q already exists", name)
		}
		if t.LocalIP == "127.0.0.1" && t.LocalPort >= port {
			port = t.LocalPort + 1
		}
	}
	tc := TrunkConfig{
		Session:    name,
		LocalIP:    "127.0.0.1",
		LocalPort:  port,
		Username:   name,
		Password:   password,
		Register:   true,
		DID:        did,
		CallerName: "WhatsApp " + name,
	}
	c.Trunks = append(c.Trunks, tc)
	if err := c.applyDefaults(); err != nil {
		c.Trunks = c.Trunks[:len(c.Trunks)-1]
		c.mu.Unlock()
		return nil, err
	}
	added := c.Trunks[len(c.Trunks)-1]
	c.mu.Unlock()

	if err := c.Save(); err != nil {
		return nil, err
	}
	return &added, nil
}

// Remove drops the trunk of a session (by name or id) and persists the file.
// Removing a trunk that does not exist is not an error.
func (c *Config) Remove(sessionName, sessionID string) error {
	if c == nil || c.path == "" {
		return nil
	}
	c.mu.Lock()
	kept := c.Trunks[:0]
	removed := false
	for _, t := range c.Trunks {
		if strings.EqualFold(t.Session, sessionName) || t.Session == sessionID {
			removed = true
			continue
		}
		kept = append(kept, t)
	}
	c.Trunks = kept
	c.mu.Unlock()
	if !removed {
		return nil
	}
	return c.Save()
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
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
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.Trunks {
		t := c.Trunks[i]
		if strings.EqualFold(t.Session, sessionName) || t.Session == sessionID {
			return &t
		}
	}
	return nil
}
