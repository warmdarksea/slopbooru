# imgrec's models: every weight tree pinned by revision and NAR hash, and
# models.json, the one file the Go side needs from nix -- each model's
# weights path and the store path closure that produced it (closureInfo at
# build time, so recording a value's provenance needs no nix at runtime).
#
# The parameters (what to ask, gpu share, concurrency) are the specs'
# (../specs/*.json); gpu/seqs here are the python pipeline's leftovers,
# kept so the manifest stays byte-identical to llm-qwen's.
#
# These total ~75 GB. Prime the store with a resumable download (curl -C -
# per file, then `nix store add --mode nar`) rather than letting a
# fixed-output fetch start from zero.
{ pkgs, fetchHuggingFace }:
let
  inherit (pkgs) lib;
  hf = fetchHuggingFace; # lib/models.nix's: already bound to a pkgs

  fleet = {
    # qwen3.8 27b, awq int4 (compressed-tensors, w4a16): vllm reads the
    # safetensors as-is. chat_template.jinja ships as its own file, and
    # without it /v1/chat/completions has no template. ~21 GB.
    qwen = hf {
      src = "hf:cyankiwi/Qwen3.8-27B-AWQ-INT4@e3a700906e6a73ba4290b1873dc2ba8198527ab5";
      files = [ "*.safetensors" "*.json" "merges.txt" "chat_template.jinja" ];
      hash = "sha256-C8A2uEgXb06K1DABr0wq+1Jnl0/ynZISf/xsT0vhcyM=";
    };

    # gemma 4 26B-A4B instruct, awq int4: MoE, ~3.8B active -- the same
    # fields as qwen at twice the speed
    gemma = hf {
      src = "hf:axteam/gemma-4-26B-A4B-it-AWQ-4bit@8c3be456947020ed8829eaa9687ff9aa7d355fd2";
      files = [ "model.safetensors" "*.json" "*.jinja" ];
      hash = "sha256-tBbiJzXfMOGhUUoWyeHfmTn5Ah83DFkgFx3pz8lhnOA=";
    };

    # toriigate v0.4 7B (qwen2-vl): anime-focused captioner, bf16
    toriigate = hf {
      src = "hf:Minthy/ToriiGate-v0.4-7B@3a2bd48da3a1ea95d6853066fbe863481ca8b507";
      files = [ "*.safetensors" "*.json" "merges.txt" ];
      hash = "sha256-LRTvW2OfZpCMGpbzXzxVxHY1keV+Tv82oYGQOJa0Vn0=";
    };

    # joycaption beta one (llava on llama-3.1-8B), fp8-dynamic (marlin w8a16
    # on ampere). The same author's GPTQ-4bit quant loads but generates
    # whitespace garbage under vllm 0.33 -- do not go back.
    joycaption = hf {
      src = "hf:NeoChen1024/llama-joycaption-beta-one-hf-llava-FP8-Dynamic@44215bf5852d075aa935d1779fd7b25abc6a3448";
      files = [ "*.safetensors" "*.json" "*.jinja" ];
      hash = "sha256-uaq+v4KDB/6M4CRrTaTjhscVKPHfvuFuXyfpz/zlUVI=";
    };

    # deepseek-ocr 3B: text with grounding (bounding boxes). vllm has its
    # own implementation; the repo's python is never executed.
    deepseek-ocr = hf {
      src = "hf:deepseek-ai/DeepSeek-OCR@9f30c71f441d010e5429c532364a86705536c53a";
      files = [ "*.safetensors" "*.json" "*.py" ];
      hash = "sha256-CxNPC1ulbeVJmefPPk0dWoLwhtkbTBoKLVNg0GYzuoU=";
    };

    # llama-guard 4 12B, w4a16 (RedHatAI's requant of the gated meta repo;
    # this mirror is ungated): safety verdicts and hazard categories
    llamaguard = hf {
      src = "hf:RedHatAI/Llama-Guard-4-12B-quantized.w4a16@9e9f5d017893ebae6dde408ee257c53bf269d388";
      files = [ "*.safetensors" "*.json" "*.jinja" ];
      hash = "sha256-H5qgendpuTQmhPh4qBt9qfco4ahvoOMp45FPhxmJuRw=";
    };

    # pixai-tagger v1.0: a multi-label classifier over 30877 danbooru tags
    # (general/character/copyright/rating) -- tags, subjects, sources.
    # trust_remote_code runs tagger_pipeline.py out of this checkout.
    pixai = hf {
      src = "hf:pixai-labs/pixai-tagger-v1.0@9fe10addf9326e292da8a85a98ea74cd91b41771";
      files = [ "model.safetensors" "*.json" "tagger_pipeline.py" ];
      hash = "sha256-Kw48r+laYjTUzlZaJyS9iCgdy9COMpLz6JLtWPYcPSU=";
    };
  };

  legacy = {
    qwen = { mode = "fields"; gpu = 0.95; seqs = 8; mm_max_pixels = true; };
    gemma = { mode = "fields"; gpu = 0.95; seqs = 8; };
    toriigate = { mode = "caption"; gpu = 0.90; mm_max_pixels = true; };
    joycaption = { mode = "caption"; gpu = 0.60; };
    "deepseek-ocr" = { mode = "ocr"; gpu = 0.32; };
    llamaguard = { mode = "guard"; gpu = 0.90; };
    pixai = { mode = "tagger"; gpu = 0; };
  };
in
{
  inherit fleet;
  manifest = pkgs.writeText "models.json" (builtins.toJSON {
    models = lib.mapAttrs (name: spec: spec // {
      path = "${fleet.${name}}";
      closure = "${pkgs.closureInfo { rootPaths = [ fleet.${name} ]; }}/store-paths";
    }) legacy;
  });
}
