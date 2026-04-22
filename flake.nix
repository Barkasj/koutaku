{
  description = "Koutaku's Nix Flake";

  # Flake inputs
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/staging-next";
  };

  # Flake outputs
  outputs =
    { self, ... }@inputs:
    let
      # The systems supported for this flake's outputs
      supportedSystems = [
        "x86_64-linux" # 64-bit Intel/AMD Linux
        "aarch64-linux" # 64-bit ARM Linux
        "aarch64-darwin" # 64-bit ARM macOS
      ];

      # Helper for providing system-specific attributes
      forEachSupportedSystem =
        f:
        inputs.nixpkgs.lib.genAttrs supportedSystems (
          system:
          f {
            inherit system;
            # Provides a system-specific, configured Nixpkgs
            pkgs = import inputs.nixpkgs {
              inherit system;
              # Enable using unfree packages
              config.allowUnfree = true;
            };
          }
        );
    in
    {
      nixosModules = {
        koutaku =
          { pkgs, lib, ... }:
          {
            imports = [ ./nix/modules/koutaku.nix ];
            services.koutaku.package = lib.mkDefault self.packages.${pkgs.system}.koutaku-http;
          };
      };

      packages = forEachSupportedSystem (
        {
          pkgs,
          system,
        }:
        let
          version = "1.4.9";

          koutaku-ui = pkgs.callPackage ./nix/packages/koutaku-ui.nix {
            src = self;
            inherit version;
          };
        in
        {
          koutaku-ui = koutaku-ui;

          koutaku-http = pkgs.callPackage ./nix/packages/koutaku-http.nix {
            inherit inputs;
            src = self;
            inherit version;
            inherit koutaku-ui;
          };

          default = self.packages.${system}.koutaku-http;
        }
      );

      apps = forEachSupportedSystem (
        { system, ... }:
        {
          koutaku-http = {
            type = "app";
            program = "${self.packages.${system}.koutaku-http}/bin/koutaku-http";
          };

          default = self.apps.${system}.koutaku-http;
        }
      );

      # To activate the default environment:
      # nix develop
      # Or if you use direnv:
      # direnv allow
      devShells = forEachSupportedSystem (
        { pkgs, ... }:
        {
          # Run `nix develop` to activate this environment or `direnv allow` if you have direnv installed
          default = import ./nix/devshells/default.nix { inherit pkgs; };
        }
      );
    };
}
