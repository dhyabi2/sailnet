package relay

import (
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/dhyabi2/sail/nano"
)

// StaticRelay is a relay descriptor exchanged out-of-band (Docker/LAN tests,
// or a bootstrap file shared with a friend) instead of read from the ledger.
type StaticRelay struct {
	Account string `json:"account"`
	Country string `json:"country"`
	ASN     uint32 `json:"asn"`
	MinRate uint32 `json:"minRate"`
	Flags   uint16 `json:"flags"`
	IP      string `json:"ip"`
	Port    uint16 `json:"port"`
	CertFP  string `json:"certFP"` // 12 hex
	Host    string `json:"host,omitempty"`
}

// WriteStatic publishes this relay's descriptor as JSON.
func WriteStatic(path string, ri *RelayInfo) error {
	s := StaticRelay{Account: ri.Account, Country: ri.Country, ASN: ri.ASN, MinRate: ri.MinRate, Flags: ri.Flags, IP: ri.Desc.IP.String(), Port: ri.Desc.Port, CertFP: hex.EncodeToString(ri.Desc.CertFP[:]), Host: ri.Host}
	data, _ := json.MarshalIndent(s, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadDir replaces the registry with every *.json descriptor in dir.
func (r *Registry) LoadDir(dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	m := map[string]*RelayInfo{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s StaticRelay
		if json.Unmarshal(data, &s) != nil || s.Account == "" {
			continue
		}
		pub, err := nano.AddressToPubkey(s.Account)
		if err != nil {
			continue
		}
		fp, err := hex.DecodeString(strings.TrimSpace(s.CertFP))
		if err != nil || len(fp) != 6 {
			continue
		}
		ri := &RelayInfo{Account: s.Account, Pub: pub, Country: s.Country, ASN: s.ASN, MinRate: s.MinRate, Flags: s.Flags, Desc: Descriptor{IP: net.ParseIP(s.IP), Port: s.Port}, Host: s.Host}
		copy(ri.Desc.CertFP[:], fp)
		m[s.Account] = ri
	}
	r.mu.Lock()
	r.relays = m
	r.mu.Unlock()
	return nil
}
