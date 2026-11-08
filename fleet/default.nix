# The fleet: the models slopbooru asks about pictures, as gpu-timeshare
# models. Nix-only: the engine is a virtualenv of ~200 cuda wheels that
# only nix keeps reproducible.
#
#   nix-build -A fleet.engine          the vllm virtualenv (vllm, torch, the tagger's deps)
#   nix-build -A fleet.timeshare-toml  a timeshare.toml running every model as a child process
#   fleet.models, fleet.settings       the same, for services.gpu-timeshare.{models,settings}
#
# Everything about *serving* a model lives here: weights, engine, vllm's
# settings, its share of the card. Everything about *asking* it (prompt,
# schema, sampling, downscaling) is slopbooru's (classify.toml).
{ pkgs, fetchHuggingFace }:
let
  inherit (pkgs) lib;
  engine = import ./engine.nix { inherit pkgs; };
  weights = (import ./models.nix { inherit pkgs fetchHuggingFace; }).fleet;

  # vllm with every runtime fix the wheels need on nix (engine.nix)
  vllm = pkgs.writeShellApplication {
    name = "fleet-vllm";
    runtimeInputs = [ pkgs.coreutils ];
    # engine.env sets both $vllm_bin and $engine_python; each wrapper uses one
    excludeShellChecks = [ "SC2034" ];
    text = ''
      ${engine.env}
      exec "$vllm_bin" "$@"
    '';
  };

  pixai-server = pkgs.writeShellApplication {
    name = "pixai-server";
    runtimeInputs = [ pkgs.coreutils ];
    # engine.env sets both $vllm_bin and $engine_python; each wrapper uses one
    excludeShellChecks = [ "SC2034" ];
    text = ''
      ${engine.env}
      exec "$engine_python" ${./pixai_server.py} "$@"
    '';
  };

  # a vllm setting as a flag: max_model_len -> --max-model-len 8192, a
  # table -> its JSON, true -> the bare flag
  flags = settings: lib.concatLists (lib.mapAttrsToList (k: v:
    let flag = "--" + lib.replaceStrings [ "_" ] [ "-" ] k; in
    if lib.isAttrs v || lib.isList v then [ flag (builtins.toJSON v) ]
    else if v == true then [ flag ]
    else if v == false then [ ]
    else [ flag (toString v) ]) settings);

  common = {
    max_model_len = 8192;
    # nothing inherited from the weights' generation_config.json: the
    # request's sampling is all there is
    generation_config = "vllm";
    limit_mm_per_prompt = { image = 1; video = 0; };
  };
  # gemma degenerated into JSON whitespace until max_tokens (11 of 17 on
  # the card) without this; the backend must be named, "auto" fails
  # vllm's validation
  structured = { structured_outputs_config = { backend = "xgrammar"; disable_any_whitespace = true; }; };

  # settings: what changes the answer (they go in the fingerprint, so in
  # gpu-timeshare's cache key); cost/seqs: how it shares the card
  served = name: { cost, seqs ? 4, settings ? { } }:
    let
      w = weights.${name};
      s = common // settings;
    in
    {
      command = [ "${vllm}/bin/fleet-vllm" "serve" "${w}" "--served-model-name" name "--uds" "{socket}"
        "--gpu-memory-utilization" (builtins.toJSON cost) "--max-num-seqs" (toString seqs) ] ++ flags s;
      inherit cost;
      concurrency = seqs;
      # the weights, and a hash of the settings that change the answer
      fingerprint = "${w}#${builtins.substring 0 16 (builtins.hashString "sha256" (builtins.toJSON s))}";
    };

  # the rest of the config, for services.gpu-timeshare.settings (or the
  # toml below): the big models take the card an hour at a time when more
  # than one has work, and vllm may take a while to load one
  settings = {
    scheduler = { rotation = "1h"; start_timeout = "20m"; };
  };

  models = {
    qwen = served "qwen" { cost = 0.95; seqs = 8;
      settings = structured // { mm_processor_kwargs = { max_pixels = 2500000; }; }; };
    gemma = served "gemma" { cost = 0.95; seqs = 8; settings = structured; };
    toriigate = served "toriigate" { cost = 0.90;
      settings = { mm_processor_kwargs = { max_pixels = 2500000; }; }; };
    joycaption = served "joycaption" { cost = 0.60; };
    deepseek-ocr = served "deepseek-ocr" { cost = 0.32; };
    llamaguard = served "llamaguard" { cost = 0.90; };
    pixai = {
      command = [ "${pixai-server}/bin/pixai-server" "--model" "${weights.pixai}" "--uds" "{socket}"
        "--name" "pixai" "--batch" "16" ];
      cost = 0.15;
      # requests in flight: the server batches them (up to 16)
      concurrency = 32;
      fingerprint = "${weights.pixai}#${builtins.substring 0 16 (builtins.hashString "sha256" (builtins.readFile ./pixai_server.py))}";
    };
  };
in
{
  inherit engine weights vllm pixai-server models settings;
  vllm-env = engine.vllm-env;
  lock-engine = engine.lock;

  # every model as a child process of gpu-timeshare (no systemd needed):
  #   gpu-timeshare serve --config $(nix-build -A fleet.timeshare-toml)
  # with state_dir the directory it is run in
  timeshare-toml = (pkgs.formats.toml { }).generate "timeshare.toml" (settings // { inherit models; });
}
