# the fleet's engine: vllm (and the pixai tagger's torch) from upstream's
# wheels, as one immutable virtualenv.
#
# nixpkgs' vllm says `broken = cudaSupport`, so building a cuda vllm from
# source is a dead end. Instead: upstream's official wheels (cuda kernels
# precompiled, the cuda runtime vendored through the nvidia-* deps),
# consumed through nix. ./engine/pyproject.toml names `vllm==x.y.z`,
# ./engine/uv.lock pins every wheel by url and hash, and uv2nix builds the
# lot into one virtualenv. It runs on nix's python (manylinux pythons need
# /lib64) and finds libstdc++/zlib/libcuda through wheelLibPath at runtime.
#
# Every fix below was found on a real card (an RTX 3090, 2026-11-08); each
# says what broke without it. Don't "simplify" them away.
{ pkgs }:
let
  inherit (pkgs) lib;

  # the uv2nix stack, pinned (the revisions llm-qwen's flake.lock had when
  # this env was first built and run)
  github = owner: repo: rev: sha256:
    builtins.fetchTarball {
      url = "https://github.com/${owner}/${repo}/archive/${rev}.tar.gz";
      inherit sha256;
    };
  pyproject-nix = import (github "pyproject-nix" "pyproject.nix"
    "bf7d4b1d6de61db02fd4e748395634f03006e5de"
    "sha256-NPWgb6H4arnWyTKvYIys5U0xqd5QY74Z+lyTJA+7XGY=") { inherit lib; };
  uv2nix = import (github "pyproject-nix" "uv2nix"
    "c361746c7d630d98b972378290547c8d35542a7a"
    "sha256-3CE1CEwprYlHRlRVkXDkhqec0jjF9Cti7tdk8vVDsCM=") { inherit lib pyproject-nix; };
  build-systems = import (github "pyproject-nix" "build-system-pkgs"
    "461a6a8540307fb9c05b8f6ad29e9720d2a621db"
    "sha256-Fzdew7s0V4PHsBX0GDQ+1Zcd4HejuqAh9rAjSP9M6JQ=") { inherit lib pyproject-nix uv2nix; };

  workspace = uv2nix.lib.workspace.loadWorkspace { workspaceRoot = ./engine; };

  # gpu wheels link libraries that only exist at runtime -- libcuda (the
  # driver), libtorch*/libc10* (the torch wheel, preloaded at import), and
  # the cuda libraries the nvidia-* wheels ship to each other. auto-patchelf
  # is told to leave those unresolved. After a lock bump,
  # `nix-build -A fleet.engine --keep-going` names every wheel that needs
  # adding here, in one pass.
  relaxWheels = names: _final: prev:
    lib.genAttrs names (name:
      prev.${name}.overrideAttrs (_: { autoPatchelfIgnoreMissingDeps = true; }));

  # wheels that ship packaging leftovers at the top of site-packages, where
  # they collide in the venv: flashinfer-python and torch-c-dlpack-ext both
  # carry their build-time build_backend.py. nothing imports it.
  dropStrays = strays: _final: prev:
    lib.mapAttrs (name: files:
      prev.${name}.overrideAttrs (old: {
        postInstall = (old.postInstall or "") + lib.concatMapStrings (f: ''
          rm -f $out/lib/python*/site-packages/${f}
        '') files;
      })) strays;

  # humming-kernels' nvrtc_compile (joycaption's fp8 kernels) segfaults once
  # auto-patchelf has rewritten it (exit -11, no output); the same binary
  # with only its loader set works. Keep a pristine copy before fixup, let
  # auto-patchelf do the rest of the wheel, and put the copy back in a phase
  # that runs after every other (postFixup would run *before*
  # auto-patchelf). Its libstdc++ comes from the wrapper's LD_LIBRARY_PATH.
  #
  # (No interpreter patching for triton's ptxas & co: auto-patchelf already
  # gives them nix's loader, and patching ptxas a second time corrupts it --
  # it segfaults, and triton reports "cannot find ptxas".)
  hummingNvrtc = _final: prev: {
    humming-kernels = prev.humming-kernels.overrideAttrs (old: {
      nativeBuildInputs = (old.nativeBuildInputs or [ ]) ++ [ pkgs.patchelf ];
      postInstall = (old.postInstall or "") + ''
        for f in $out/lib/python*/site-packages/humming/_native/*/nvrtc_compile; do
          cp "$f" "$NIX_BUILD_TOP/pristine-$(basename "$(dirname "$f")")"
        done
      '';
      postPhases = (old.postPhases or [ ]) ++ [ "restoreNvrtcCompile" ];
      restoreNvrtcCompile = ''
        for f in $out/lib/python*/site-packages/humming/_native/*/nvrtc_compile; do
          chmod u+w "$f"
          cp "$NIX_BUILD_TOP/pristine-$(basename "$(dirname "$f")")" "$f"
          patchelf --set-interpreter ${pkgs.glibc}/lib/ld-linux-x86-64.so.2 "$f"
          chmod 555 "$f"
        done
      '';
    });
  };

  pythonSet =
    (pkgs.callPackage pyproject-nix.build.packages { python = pkgs.python3; }).overrideScope
      (lib.composeManyExtensions [
        build-systems.default
        (workspace.mkPyprojectOverlay { sourcePreference = "wheel"; })
        hummingNvrtc
        (dropStrays {
          flashinfer-python = [ "build_backend.py" ];
          torch-c-dlpack-ext = [ "build_backend.py" ];
        })
        (relaxWheels [
          "humming-kernels"
          "nccl-extensions"
          "numba"
          "nvidia-cufile"
          "nvidia-cusolver"
          "nvidia-cusparse"
          "nvidia-cutlass-dsl-libs-base"
          "nvidia-nvshmem-cu13"
          "pynvvideocodec"
          "tilelang"
          "torch"
          "torch-c-dlpack-ext"
          "torchaudio"
          "torchcodec"
          "torchvision"
          "vllm"
          "xgrammar"
        ])
      ]);

  vllm-env = pythonSet.mkVirtualEnv "vllm-env" workspace.deps.default;

  wheelLibPath = lib.makeLibraryPath [ pkgs.stdenv.cc.cc.lib pkgs.zlib ] + ":/run/opengl-driver/lib";
in
{
  inherit vllm-env;

  # shell for a wrapper: the engine's runtime fixes, then $vllm_bin and
  # $engine_python. IMGREC_ENGINE (a virtualenv with bin/vllm) swaps the
  # engine for experiments.
  env = ''
    engine=''${IMGREC_ENGINE:-${vllm-env}}
    vllm_bin=$engine/bin/vllm
    engine_python=$engine/bin/python

    # nvrtc lives in a wheel
    nvrtc_dir=
    for d in "$engine"/lib/python*/site-packages/nvidia/cu13/lib; do
      [ -d "$d" ] && nvrtc_dir=$d
    done
    # every nvidia-* wheel's lib dir: the wheels find their own
    # sub-libraries through rpath $ORIGIN (cudnn dlopens cudnn_graph,
    # cudnn_ops... by name), and auto-patchelf replaces that rpath -- cudnn
    # then loads and fails at its first call ("cannot load symbol
    # cudnnGetVersion"). The loader path puts back what $ORIGIN gave them.
    nvidia_libs=
    for d in "$engine"/lib/python*/site-packages/nvidia/*/lib; do
      [ -d "$d" ] && nvidia_libs=''${nvidia_libs:+$nvidia_libs:}$d
    done

    # the engine brings its own cuda. A toolkit from the surrounding
    # environment is a version skew waiting to happen: humming-kernels
    # prefers $CUDA_PATH/$CUDA_HOME, took a newer nvrtc than the driver,
    # and segfaulted compiling joycaption's fp8 kernels.
    unset CUDA_HOME CUDA_PATH NVCC_PREPEND_FLAGS

    # nothing may touch the network at runtime: vllm's usage reporting is
    # on by default upstream (a report to stats.vllm.ai on every start),
    # and huggingface would revalidate a local checkout against the hub
    export HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1 HF_DATASETS_OFFLINE=1 \
      HF_HUB_DISABLE_TELEMETRY=1 VLLM_NO_USAGE_STATS=1 DO_NOT_TRACK=1

    export LD_LIBRARY_PATH=${wheelLibPath}''${nvrtc_dir:+:$nvrtc_dir}''${nvidia_libs:+:$nvidia_libs}
    export LIBRARY_PATH=/run/opengl-driver/lib
    export VLLM_USE_FLASHINFER_SAMPLER=''${IMGREC_FLASHINFER:-0}
    # triton finds libcuda without ldconfig; kernels jit-compile with $CC
    export TRITON_LIBCUDA_PATH=/run/opengl-driver/lib
    export TRITON_ALLOW_NON_CONSTEXPR_GLOBALS=1
    export CC=${pkgs.gcc14}/bin/gcc
    export PATH=${lib.makeBinPath [ pkgs.ninja pkgs.gcc14 ]}:$PATH
  '';

  # re-resolve ./engine/pyproject.toml into uv.lock after a version bump
  # (network: an app, not a derivation). Run it in fleet/engine.
  lock = pkgs.writeShellApplication {
    name = "lock-engine";
    runtimeInputs = [ pkgs.uv ];
    text = ''uv lock --python ${pkgs.python3}/bin/python3 "$@"'';
  };
}
