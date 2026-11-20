{
  description = "Pull-through Decky Loader plugin store that adds plugins from GitHub releases";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts = {
      url = "github:hercules-ci/flake-parts";
      inputs.nixpkgs-lib.follows = "nixpkgs";
    };
  };

  outputs =
    inputs@{ flake-parts, self, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];

      flake = {
        nixosModules = {
          decky-store-proxy = ./nix/module.nix;
          default = self.nixosModules.decky-store-proxy;
        };
        overlays.default = final: _prev: {
          decky-store-proxy = final.callPackage ./nix/package.nix { };
        };
      };

      perSystem =
        {
          pkgs,
          self',
          system,
          ...
        }:
        {
          packages = {
            decky-store-proxy = pkgs.callPackage ./nix/package.nix { };
            default = self'.packages.decky-store-proxy;
          };

          # Evaluates the NixOS module and builds the resulting systemd unit.
          checks.module =
            (inputs.nixpkgs.lib.nixosSystem {
              inherit system;
              modules = [
                self.nixosModules.default
                {
                  boot.isContainer = true;
                  system.stateVersion = "25.11";
                  services.decky-store-proxy = {
                    enable = true;
                    settings = {
                      orgs = [ "example-org" ];
                      extraRepos = [ "someone/some-plugin" ];
                      refreshInterval = "30m";
                      githubTokenFile = "/run/secrets/github-token";
                    };
                  };
                }
              ];
            }).config.systemd.units."decky-store-proxy.service".unit;

          devShells.default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.gopls
            ];
          };

          formatter = pkgs.nixfmt;
        };
    };
}
