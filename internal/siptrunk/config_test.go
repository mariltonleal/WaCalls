package siptrunk

import (
	"os"
	"path/filepath"
	"testing"

	"wacalls/internal/voip/core"

	"github.com/emiago/sipgo/sip"
)

func TestLoadDefaultsAndFind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trunks.json")
	body := `{"rtp_port_start":20000,"rtp_port_end":20100,"trunks":[
		{"session":"Tubras","local_port":5070,"username":"wa-tubras","password":"x","register":true,"did":"5511209331310"}
	]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	tc := cfg.Find("tubras", "")
	if tc == nil {
		t.Fatal("trunk not found by case-insensitive name")
	}
	if tc.LocalIP != "127.0.0.1" || tc.PBXHost != "127.0.0.1" || tc.PBXPort != 5060 || tc.MaxCalls != 1 || tc.RingTimeoutSec != 60 || tc.Codecs != "alaw,ulaw" {
		t.Fatalf("defaults not applied: %+v", *tc)
	}
	if cfg.Find("verdepack", "") != nil {
		t.Fatal("unexpected match")
	}
}

func TestLoadRejectsDuplicatePort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trunks.json")
	body := `{"trunks":[
		{"session":"a","local_port":5070,"did":"1"},
		{"session":"b","local_port":5070,"did":"2"}
	]}`
	_ = os.WriteFile(path, []byte(body), 0o600)
	if _, err := Load(path); err == nil {
		t.Fatal("expected duplicate port error")
	}
}

func TestLoadEmptyPath(t *testing.T) {
	cfg, err := Load("")
	if err != nil || cfg == nil || len(cfg.Trunks) != 0 {
		t.Fatalf("empty path should yield empty config, got %v %v", cfg, err)
	}
	if cfg.Find("x", "y") != nil {
		t.Fatal("empty config must not match")
	}
}

func TestSipStatusForReason(t *testing.T) {
	cases := map[core.EndCallReason]int{
		core.EndCallReasonDeclined:  603,
		core.EndCallReasonBusy:      sip.StatusBusyHere,
		core.EndCallReasonTimeout:   sip.StatusTemporarilyUnavailable,
		core.EndCallReasonCancelled: sip.StatusRequestTerminated,
		core.EndCallReasonFailed:    sip.StatusServiceUnavailable,
		core.EndCallReasonUnknown:   sip.StatusServiceUnavailable,
	}
	for reason, want := range cases {
		if got, _ := sipStatusForReason(reason); got != want {
			t.Errorf("%s: got %d want %d", reason, got, want)
		}
	}
}

func TestDigits(t *testing.T) {
	if got := digits("+55 (19) 9876-5432"); got != "551998765432" {
		t.Fatalf("got %q", got)
	}
}
