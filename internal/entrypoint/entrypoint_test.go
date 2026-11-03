package entrypoint

import "testing"

func TestFromUserAgent(t *testing.T) {
	for ua, want := range map[string]string{
		"claude-cli/2.1.316 (external, sdk-cli)":                   "sdk-cli",
		"claude-cli/2.1.316 (external, sdk-ts, agent-sdk/0.3.316)": "sdk-ts",
		"claude-cli/2.1.316 (external, cli)":                       "cli",
		"claude-cli/2.1.316 (external, hex)":                       "hex",
		"curl/8.0":                                                 "",
	} {
		if got := FromUserAgent(ua); got != want {
			t.Errorf("FromUserAgent(%q) = %q, want %q", ua, got, want)
		}
	}
	if Weight("sdk-py", 1.7) != 1.7 || Weight("t3-code", 1.7) != 1 {
		t.Error("only Agent SDK entrypoints carry the factor")
	}
}
