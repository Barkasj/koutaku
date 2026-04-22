{
  pkgs,
  inputs,
  src,
  version,
  koutaku-ui,
}:
let
  lib = pkgs.lib;

  # Koutaku requires Go 1.26 (go.mod/go.work). Force Go 1.26 for buildGoModule.
  buildGoModule = pkgs.callPackage "${inputs.nixpkgs}/pkgs/build-support/go/module.nix" {
    go = pkgs.go_1_26 or pkgs.go;
  };

  transportsLocalReplaces = ''
    if [ -f transports/go.mod ]; then
      cat >> transports/go.mod <<'EOF'

    replace github.com/koutaku/koutaku/core => ../core
    replace github.com/koutaku/koutaku/framework => ../framework
    replace github.com/koutaku/koutaku/plugins/governance => ../plugins/governance
    replace github.com/koutaku/koutaku/plugins/compat => ../plugins/compat
    replace github.com/koutaku/koutaku/plugins/logging => ../plugins/logging
    replace github.com/koutaku/koutaku/plugins/otel => ../plugins/otel
    replace github.com/koutaku/koutaku/plugins/semanticcache => ../plugins/semanticcache
    replace github.com/koutaku/koutaku/plugins/telemetry => ../plugins/telemetry
    EOF
    fi
  '';
in
buildGoModule {
  pname = "koutaku-http";
  inherit version;
  inherit src;

  modRoot = "transports";
  subPackages = [ "koutaku-http" ];
  vendorHash = "sha256-Ck1cwv/DYI9EXmp7U2ZSNXlU+Qok8BFn5bcN1Pv7Nmc=";

  doCheck = false;

  overrideModAttrs = final: prev: {
    postPatch = (prev.postPatch or "") + transportsLocalReplaces;
  };

  env = {
    CGO_ENABLED = "1";
  };

  nativeBuildInputs = with pkgs; [
    pkg-config
    gcc
  ];
  buildInputs = [ pkgs.sqlite ];

  postPatch = transportsLocalReplaces;

  preBuild = ''
    # Provide UI assets for //go:embed all:ui
    rm -rf koutaku-http/ui
    mkdir -p koutaku-http/ui
    if [ -d "${koutaku-ui}/ui" ]; then
      cp -R --no-preserve=mode,ownership,timestamps "${koutaku-ui}/ui/." koutaku-http/ui/
    else
      printf '%s\n' '<!doctype html><meta charset="utf-8"><title>Koutaku</title>' > koutaku-http/ui/index.html
    fi
  '';

  ldflags = [
    "-s"
    "-w"
    "-X main.Version=${version}"
  ];

  meta = {
    mainProgram = "koutaku-http";
    description = "Koutaku HTTP gateway";
    homepage = "https://github.com/koutaku/koutaku";
    license = lib.licenses.asl20;
  };
}