package sim

import (
	"path/filepath"
	"strings"
	"testing"
)

func researchIndex(t *testing.T) SourceIndex {
	t.Helper()
	idx, err := LoadResearch(filepath.Join("..", "..", "vault", "RESEARCH.md"))
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestParseResearch(t *testing.T) {
	idx := researchIndex(t)
	for id, secondary := range map[string]bool{
		"R-5321": false, "R-GOOGLE-ERR": false, "R-YAHOO": false,
		"R-SUPED-YAHOO": true, "R-MS-509": true, "S-DNS": false,
	} {
		got, ok := idx[id]
		if !ok || got != secondary {
			t.Errorf("%s: got (%v, %v), want (%v, true)", id, got, ok, secondary)
		}
	}
}

// TestRepoProfiles loads the shipped profiles against RESEARCH.
func TestRepoProfiles(t *testing.T) {
	profiles, err := LoadProfiles(filepath.Join("..", "..", "profiles"), researchIndex(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gmail-model", "yahoo-model", "outlook-model", "baseline", "always-defer"} {
		if profiles[name] == nil {
			t.Errorf("profile %s missing", name)
		}
	}
	// Gmail has no readable primary guideline (D11): it must say so.
	if strings.TrimSpace(profiles["gmail-model"].Limitations) == "" {
		t.Error("gmail-model must declare limitations")
	}
	zone, err := LoadZone(filepath.Join("..", "..", "lab", "zone.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range profiles {
		for _, h := range p.Hosts {
			if len(zone.Addrs(h)) == 0 {
				t.Errorf("%s host %s has no A record in lab/zone.json", p.Name, h)
			}
		}
	}
}

const validRule = `{"id": "r1", "kind": "max_msgs_per_conn", "params": {"limit": 3},
	"on_violation": {"action": "close_silent"}, "source": "R-YAHOO", "assumption": true}`

func profileJSON(rules string, extra string) string {
	return `{"name": "p", "sources": ["R-YAHOO", "R-GOOGLE-ERR"], ` + extra +
		`"adaptation_period_s": 0, "hosts": ["mx.p.test"], "rules": [` + rules + `]}`
}

func withSecondary(rules, extra string) string {
	return strings.Replace(profileJSON(rules, extra), `"R-GOOGLE-ERR"]`, `"R-GOOGLE-ERR", "R-SUPED-YAHOO"]`, 1)
}

func TestProfileValidation(t *testing.T) {
	idx := researchIndex(t)
	cases := []struct {
		name    string
		profile string
		wantErr string
	}{
		{"valid", profileJSON(validRule, ""), ""},
		{"no source and not assumption",
			profileJSON(`{"id": "r", "kind": "max_msgs_per_conn", "params": {"limit": 3},
				"on_violation": {"action": "close_silent"}, "assumption": false}`, ""),
			"no source and not marked assumption: true"},
		{"no source, assumption missing",
			profileJSON(`{"id": "r", "kind": "max_msgs_per_conn", "params": {"limit": 3},
				"on_violation": {"action": "close_silent"}}`, ""),
			"assumption is required"},
		{"no source but assumption true is fine",
			profileJSON(`{"id": "r", "kind": "max_msgs_per_conn", "params": {"limit": 3},
				"on_violation": {"action": "close_silent"}, "assumption": true}`, ""),
			""},
		{"response outside 3.2",
			profileJSON(`{"id": "r", "kind": "rate", "params": {"scope": "src_ip", "limit": 1, "per_s": 1},
				"on_violation": {"action": "respond", "response": {"code": 450, "enhanced": "4.7.1", "text": "Slow down"}},
				"source": "R-GOOGLE-ERR", "assumption": true}`, ""),
			"not in contract table 3.2"},
		{"respond with altered text",
			profileJSON(`{"id": "r", "kind": "respond", "params": {"stage": "rcpt", "match": {},
				"response": {"code": 550, "enhanced": "5.1.1", "text": "No such user"}},
				"source": "R-GOOGLE-ERR", "assumption": false}`, ""),
			"not in contract table 3.2"},
		{"engine-only 556 is rejected",
			profileJSON(`{"id": "r", "kind": "respond", "params": {"stage": "rcpt", "match": {},
				"response": {"code": 556, "enhanced": "5.1.10", "text": "Requested action not taken: domain does not accept mail"}},
				"source": "R-GOOGLE-ERR", "assumption": false}`, ""),
			"not in contract table 3.2"},
		{"secondary response without limitations",
			withSecondary(`{"id": "r", "kind": "rate", "params": {"scope": "src_ip", "limit": 1, "per_s": 1},
				"on_violation": {"action": "respond_close", "response": {"code": 421, "enhanced": "", "text": "[IPTS04] Message deferred for IP policy reasons"}},
				"source": "R-YAHOO", "assumption": true}`, ""),
			"limitations is empty"},
		{"secondary response with limitations",
			withSecondary(`{"id": "r", "kind": "rate", "params": {"scope": "src_ip", "limit": 1, "per_s": 1},
				"on_violation": {"action": "respond_close", "response": {"code": 421, "enhanced": "", "text": "[IPTS04] Message deferred for IP policy reasons"}},
				"source": "R-YAHOO", "assumption": true}`, `"limitations": "secondary text", `),
			""},
		{"421 must close",
			profileJSON(`{"id": "r", "kind": "rate", "params": {"scope": "src_ip", "limit": 1, "per_s": 1},
				"on_violation": {"action": "respond", "response": {"code": 421, "enhanced": "4.7.0", "text": "Try again later, closing connection."}},
				"source": "R-GOOGLE-ERR", "assumption": true}`, ""),
			"respond_close"},
		{"source not in profile sources",
			profileJSON(`{"id": "r", "kind": "max_msgs_per_conn", "params": {"limit": 3},
				"on_violation": {"action": "close_silent"}, "source": "R-MS-515", "assumption": true}`, ""),
			"not listed in the profile sources"},
		{"unknown source", strings.Replace(profileJSON(validRule, ""), `"R-YAHOO", "R-GOOGLE`, `"R-YAHOO", "R-NOPE", "R-GOOGLE`, 1),
			"R-NOPE is not in RESEARCH"},
		{"unknown kind", profileJSON(`{"id": "r", "kind": "teleport", "params": {}, "assumption": true}`, ""), "unknown kind"},
		{"unknown field", profileJSON(`{"id": "r", "kind": "max_msgs_per_conn", "params": {"limit": 3, "burst": 2},
				"on_violation": {"action": "close_silent"}, "assumption": true}`, ""), "unknown field"},
		{"duplicate id", profileJSON(validRule+","+validRule, ""), "duplicate id"},
		{"host outside .test", strings.Replace(profileJSON(validRule, ""), "mx.p.test", "mx.example.com", 1), "not a name under .test"},
		{"auth accepted", profileJSON(`{"id": "r", "kind": "auth", "params": {"require": ["spf"],
				"fail_response": {"code": 550, "enhanced": "5.7.26", "text": "This email has been blocked because the sender is unauthenticated."}},
				"source": "R-GOOGLE-ERR", "assumption": false}`, ""), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseProfile([]byte(tc.profile), idx)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("accepted, want error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
