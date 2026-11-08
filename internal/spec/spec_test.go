package spec

import (
	"encoding/json"
	"strings"
	"testing"
)

func load(t *testing.T) map[string]*Spec {
	t.Helper()
	specs, err := LoadDir("../../specs")
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 7 {
		t.Fatalf("want 7 specs, got %d", len(specs))
	}
	for _, s := range specs {
		s.Weights = "/nix/store/weights-" + s.Name
	}
	return specs
}

func TestCanonicalOrderFree(t *testing.T) {
	var a, b any
	json.Unmarshal([]byte(`{"b": [1, {"y": 2, "x": 1}], "a": "é<&>"}`), &a)
	json.Unmarshal([]byte(`{"a": "é<&>", "b": [1, {"x": 1, "y": 2}]}`), &b)
	if Canonical(a) != Canonical(b) || Canonical(a) != `{"a":"é<&>","b":[1,{"x":1,"y":2}]}` {
		t.Errorf("canonical: %s vs %s", Canonical(a), Canonical(b))
	}
}

func TestKey(t *testing.T) {
	specs := load(t)
	q := specs["qwen"]
	base := q.Key("abc", "")
	if base != q.Key("abc", "") {
		t.Fatal("key is not stable")
	}

	// everything that shapes the answer changes the key
	for name, change := range map[string]func(s *Spec){
		"temperature": func(s *Spec) { s.Request["temperature"] = json.Number("0.3") },
		"prompt": func(s *Spec) {
			s.Request["messages"].([]any)[0].(map[string]any)["content"].([]any)[1].(map[string]any)["text"] = "other"
		},
		"max_model_len": func(s *Spec) { s.Server["max_model_len"] = json.Number("4096") },
		"max_pixels":    func(s *Spec) { s.Input["max_pixels"] = json.Number("1000") },
		"weights":       func(s *Spec) { s.Weights = "/nix/store/other" },
	} {
		s := load(t)["qwen"]
		change(s)
		if s.Key("abc", "") == base {
			t.Errorf("changing %s did not change the key", name)
		}
	}
	if q.Key("abd", "") == base || q.Key("abc", "ctx") == base {
		t.Error("input and context must be part of the key")
	}

	// what doesn't shape the answer doesn't
	for name, change := range map[string]func(s *Spec){
		"gpu":          func(s *Spec) { s.Runner["gpu"] = json.Number("0.5") },
		"max_num_seqs": func(s *Spec) { s.Runner["max_num_seqs"] = json.Number("2") },
		"name":         func(s *Spec) { s.Name = "qwen-again" },
	} {
		s := load(t)["qwen"]
		change(s)
		if s.Key("abc", "") != base {
			t.Errorf("changing %s changed the key", name)
		}
	}
}

func TestRequestBody(t *testing.T) {
	specs := load(t)
	body := specs["llamaguard"].RequestBody("file:///x.png", "Description: a cat", "deadbeef00")
	s := Canonical(body)
	for _, want := range []string{`"url":"file:///x.png"`, `"text":"Description: a cat"`,
		`"model":"llamaguard"`, `"logprobs":true`, `"seed":`} {
		if !strings.Contains(s, want) {
			t.Errorf("guard body lacks %s: %s", want, s)
		}
	}
	if strings.Contains(Canonical(specs["llamaguard"].Request), "a cat") {
		t.Error("RequestBody modified the spec itself")
	}
	if Seed("deadbeef00") != Seed("deadbeef11") || Seed("deadbeef") == Seed("feedbeef") {
		t.Error("seed should come from the first 8 hex digits")
	}
	// every chat spec sets its sampling explicitly: nothing is inherited
	for _, s := range specs {
		if s.Kind == "tagger" {
			continue
		}
		for _, k := range []string{"temperature", "top_p", "top_k", "max_tokens"} {
			if _, ok := s.Request[k]; !ok {
				t.Errorf("%s: %s not set", s.Name, k)
			}
		}
		if s.Server["generation_config"] != "vllm" {
			t.Errorf("%s: vllm would inherit generation_config.json", s.Name)
		}
	}
}

func TestServeArgs(t *testing.T) {
	args := strings.Join(load(t)["qwen"].ServeArgs(8001, "/"), " ")
	for _, want := range []string{"/nix/store/weights-qwen", "--served-model-name qwen", "--port 8001",
		"--max-model-len 8192", "--generation-config vllm", "--gpu-memory-utilization 0.95",
		"--max-num-seqs 8", `--mm-processor-kwargs {"max_pixels":2500000}`, `--limit-mm-per-prompt {"image":1,"video":0}`} {
		if !strings.Contains(args, want) {
			t.Errorf("serve args lack %q: %s", want, args)
		}
	}
}
