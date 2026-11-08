# imgrec, the recognition backend: nix-only, because its engine is a
# virtualenv of ~200 cuda wheels that only nix keeps reproducible.
#
#   nix-build -A imgrec            imgrec, bundled with engine + models
#   nix-build -A imgrec.bin        the Go binaries alone (imgrec, imgrec-stub, pyparity)
#   nix-build -A imgrec.engine     the vllm virtualenv
#   nix-build -A imgrec.manifest   models.json (builds every model: ~75 GB)
{
  pkgs,
  src,
  vendorHash,
  version,
  fetchHuggingFace,
}:
let
  engine = import ./engine.nix { inherit pkgs; };
  models = import ./models.nix { inherit pkgs fetchHuggingFace; };

  # the Go side; every imgrec package's tests run in the build, with
  # imgrec-stub standing in for vllm and the tagger (no gpu)
  bin = pkgs.buildGoModule {
    pname = "imgrec";
    inherit version src vendorHash;
    env.CGO_ENABLED = "0";
    subPackages = [ "imgrec/cmd/imgrec" "imgrec/cmd/imgrec-stub" "imgrec/cmd/pyparity" ];
    ldflags = [ "-s" "-w" ];
    checkPhase = ''
      runHook preCheck
      go test ./imgrec/...
      runHook postCheck
    '';
    meta.mainProgram = "imgrec";
  };

  # the engine's runtime fixes, then what nix knows and imgrec needs. Every
  # IMGREC_* can be overridden (every flag defaults from IMGREC_<NAME>).
  #   imgrec run --db pics/imgrec.db pics
  #   imgrec serve --db imgrec.db
  imgrec = pkgs.writeShellApplication {
    name = "imgrec";
    runtimeInputs = [ pkgs.coreutils ];
    text = ''
      ${engine.env}
      export IMGREC_SPECS=''${IMGREC_SPECS:-${../specs}}
      export IMGREC_MANIFEST=''${IMGREC_MANIFEST:-${models.manifest}}
      export IMGREC_VLLM=''${IMGREC_VLLM:-$vllm_bin}
      export IMGREC_PIXAI=''${IMGREC_PIXAI:-"$engine_python ${../runners/pixai_worker.py}"}
      exec ${bin}/bin/imgrec "$@"
    '';
    derivationArgs.passthru = {
      inherit bin models;
      inherit (models) manifest;
      engine = engine.vllm-env;
      lock-engine = engine.lock;
    };
  };
in
imgrec
