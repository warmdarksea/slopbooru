package model

import (
	"testing"

	"slopbooru/internal/pyjson"
)

func logprobs(t *testing.T, s string) any {
	t.Helper()
	v, err := pyjson.Decode(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Shaped like a real llama-guard reply on the card: the opening newline
// is chosen at p=1, with an "S" far down its candidates, and the verdict
// is decided at the next step.
func TestGuardProbabilityUsesTheChosenVerdictToken(t *testing.T) {
	lp := logprobs(t, `{"content": [
	  {"token": "\n\n", "top_logprobs": [{"token": "\n\n", "logprob": 0}, {"token": "S", "logprob": -46.0}]},
	  {"token": "safe", "top_logprobs": [{"token": "safe", "logprob": -0.387}, {"token": "unsafe", "logprob": -1.136}, {"token": "S", "logprob": -28.8}]}
	]}`)
	p := GuardProbability(lp)
	if p == nil || p["safe"] < 0.6 || p["safe"] > 0.75 || p["unsafe"] < 0.28 || p["unsafe"] > 0.36 {
		t.Errorf("got %v, want safe ~0.68, unsafe ~0.32 (the step where 'safe' was chosen)", p)
	}
	c := Parse("guard", "llamaguard", "x", "\n\nsafe", lp, "")
	if conf := c.Fields["confidence"].(map[string]any)["verdict"].(float64); conf < 0.6 {
		t.Errorf("verdict confidence %v, want ~0.68", conf)
	}
}

func TestGuardProbabilitySplitVerdictAndNone(t *testing.T) {
	// "un" + "safe": the chosen "un" begins the verdict
	split := logprobs(t, `{"content": [{"token": "\n\n", "top_logprobs": [{"token": "\n\n", "logprob": 0}]},
	  {"token": "un", "top_logprobs": [{"token": "un", "logprob": -0.6}, {"token": "safe", "logprob": -0.8}]}]}`)
	if p := GuardProbability(split); p == nil || p["unsafe"] < 0.5 {
		t.Errorf("split verdict: %v", p)
	}
	// no chosen token is a verdict: missing, never zeros
	none := logprobs(t, `{"content": [{"token": "\n", "top_logprobs": [{"token": "\n", "logprob": -0.3}, {"token": "s", "logprob": -9}]}]}`)
	if p := GuardProbability(none); p != nil {
		t.Errorf("no verdict token: %v, want nil", p)
	}
}
