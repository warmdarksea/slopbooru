# slopbooru, for nix.
#
#   nix-build -A slopbooru          # this machine's binary
#   nix-build -A release            # linux + windows, amd64 + arm64, SHA256SUMS
#   nix-build -A tests              # unit tests, the linux and windows (wine)
#                                   # end-to-end checks
#   nix-shell                       # go, gopls, wine, sqlite
#
# Every binary is static Go with cgo off: no libc, no libsqlite3, nothing
# from the host. Building them needs nothing but go, so the windows and
# arm64 ones are cross-compiled from here.
{
  # the nixpkgs llm-qwen's flake.lock pins, so both build with one go
  pkgs ? import (builtins.fetchTarball {
    url = "https://releases.nixos.org/nixpkgs/nixpkgs-26.11pre1107841.9d3b8db48266/nixexprs.tar.zst";
    sha256 = "sha256-WqewjvVjGQ1yhAvxeZW7IKoIT08vUNfyaeZewOwyYxg=";
  }) { },
  # a command line for a reference booru to diff every page against (the
  # Python one this was ported from); null leaves that test out
  reference ? null,
}:
let
  inherit (pkgs) lib;
  version = "0.1.0";

  # only what the go build reads, so editing this file rebuilds nothing
  src = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.unions [
      ./go.mod
      ./go.sum
      ./style.css
      ./internal/fixture/schema.sql
      ./internal/fixture/schema_fts.sql
      (lib.fileset.fileFilter (f: f.hasExt "go") ./.)
    ];
  };

  # the module cache, fetched once (a fixed-output derivation): every
  # target below builds from it offline
  vendor =
    (pkgs.buildGoModule {
      pname = "slopbooru";
      inherit version src;
      vendorHash = "sha256-uTcdpSdTVPofhkgybBLEDA90pbyLDZ79cHGxcnWCSt0=";
    }).goModules;

  goEnv = ''
    export HOME=$TMPDIR GOCACHE=$TMPDIR/go-cache GOPATH=$TMPDIR/go
    export GOFLAGS="-mod=vendor -trimpath" GOPROXY=off CGO_ENABLED=0
    cp -r ${vendor} vendor
    chmod -R u+w vendor
  '';

  # one binary. brand is the name in the page header and the log lines;
  # name is the file's.
  build =
    {
      goos,
      goarch,
      brand ? "slopbooru",
      name ? brand,
    }:
    let
      exe = name + lib.optionalString (goos == "windows") ".exe";
    in
    pkgs.stdenvNoCC.mkDerivation {
      pname = "${name}-${goos}-${goarch}";
      inherit version src;
      nativeBuildInputs = [ pkgs.go ];
      dontConfigure = true;
      buildPhase = ''
        runHook preBuild
        ${goEnv}
        GOOS=${goos} GOARCH=${goarch} go build \
          -ldflags "-s -w -buildid= -X main.version=${version} -X main.brand=${brand}" \
          -o $out/bin/${exe} .
        runHook postBuild
      '';
      dontInstall = true;
      dontFixup = true; # nothing to patch: there is no interpreter or rpath
      passthru = { inherit exe; };
      meta = {
        description = "a booru over an image recognition sqlite db";
        mainProgram = name;
        platforms = lib.platforms.all;
      };
    };

  targets = {
    linux-amd64 = build { goos = "linux"; goarch = "amd64"; };
    linux-arm64 = build { goos = "linux"; goarch = "arm64"; };
    windows-amd64 = build { goos = "windows"; goarch = "amd64"; };
    windows-arm64 = build { goos = "windows"; goarch = "arm64"; };
  };

  native = build {
    goos = pkgs.go.GOOS;
    goarch = pkgs.go.GOARCH;
  };

  # the end-to-end tester, for this machine
  slopcheck = pkgs.stdenvNoCC.mkDerivation {
    pname = "slopcheck";
    inherit version src;
    nativeBuildInputs = [ pkgs.go ];
    buildPhase = ''
      ${goEnv}
      go build -o $out/bin/slopcheck ./cmd/slopcheck
    '';
    dontInstall = true;
    dontFixup = true;
  };

  check =
    name: script:
    pkgs.runCommand "slopbooru-test-${name}" { nativeBuildInputs = [ slopcheck ]; } ''
      export HOME=$TMPDIR
      ${script}
      touch $out
    '';

  tests = lib.filterAttrs (_: v: v != null) {
    unit = pkgs.stdenvNoCC.mkDerivation {
      name = "slopbooru-test-unit";
      inherit src;
      nativeBuildInputs = [ pkgs.go ];
      buildPhase = ''
        ${goEnv}
        go vet ./...
        go test ./...
        test -z "$(gofmt -l $(find . -name '*.go' -not -path './vendor/*'))"
      '';
      installPhase = "touch $out";
    };

    # the static binary, really static: no interpreter, no needed libraries
    static = check "static" ''
      ${pkgs.file}/bin/file ${targets.linux-amd64}/bin/slopbooru | tee /dev/stderr | grep -q 'statically linked'
      ! ${pkgs.binutils-unwrapped}/bin/readelf -d ${targets.linux-amd64}/bin/slopbooru | grep -q NEEDED
    '';

    linux = check "linux" "slopcheck -bin ${native}/bin/slopbooru";

    # the windows exe under wine: paths go in as Z:\..., and everything
    # the linux check asserts must hold there too
    windows =
      if pkgs.stdenv.hostPlatform.system == "x86_64-linux" then
        check "windows" ''
          export WINEPREFIX=$TMPDIR/wine WINEDEBUG=-all WINEDLLOVERRIDES="mscoree,mshtml,winemenubuilder.exe=d"
          export PATH=${pkgs.wineWow64Packages.stable}/bin:$PATH
          wineboot -i >/dev/null 2>&1 || true
          slopcheck -bin ${targets.windows-amd64}/bin/slopbooru.exe -wrap wine -winpaths
          wineserver -k || true
        ''
      else
        null;

    parity =
      if reference == null then
        null
      else
        check "parity" ''
          slopcheck -bin ${native}/bin/slopbooru -ref ${lib.escapeShellArg reference}
        '';
  };

  release = pkgs.runCommand "slopbooru-${version}" { } ''
    mkdir -p $out
    ${lib.concatStrings (
      lib.mapAttrsToList (target: drv: ''
        cp ${drv}/bin/${drv.exe} $out/slopbooru-${version}-${target}${lib.optionalString (lib.hasPrefix "windows" target) ".exe"}
      '') targets
    )}
    cd $out && ${pkgs.coreutils}/bin/sha256sum * > SHA256SUMS
  '';
in
# linux-amd64, linux-arm64, windows-amd64, windows-arm64: one binary each
targets
// {
  inherit
    build
    release
    slopcheck
    vendor
    version
    ;
  slopbooru = native;
  tests = tests // {
    all = pkgs.linkFarm "slopbooru-tests" (lib.mapAttrs (_: t: t) tests);
  };
  shell = pkgs.mkShell {
    packages = [
      pkgs.go
      pkgs.gopls
      pkgs.gotools
      pkgs.sqlite
      pkgs.curl
      pkgs.wineWow64Packages.stable
    ];
    CGO_ENABLED = "0";
    WINEDEBUG = "-all";
  };
}
