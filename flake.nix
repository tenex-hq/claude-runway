{
  description = "How much of your Claude subscription allowance is left, and whether it will last to the reset";

  # Only nixpkgs. flake-utils would be the conventional second input, but the
  # system iteration it provides is six lines to write by hand, and this project's
  # whole premise is not taking dependencies it does not need.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      version = "0.4.1";
      # Same text as the flake's own `description` above. It cannot be shared: that
      # attribute is evaluated outside this let binding.
      summary = "How much of your Claude subscription allowance is left, and whether it will last to the reset";
    in
    {
      packages = forAllSystems (pkgs: rec {
        claude-runway = pkgs.buildGoModule {
          pname = "claude-runway";
          inherit version;
          src = ./.;

          # The module has no external dependencies, so there is nothing to vendor
          # and no hash to pin. This is what stdlib-only buys.
          vendorHash = null;

          ldflags = [ "-s" "-w" "-X" "main.binVersion=${version}" ];

          # The test suite is hermetic: it touches no network and redirects HOME and
          # XDG_CACHE_HOME to a scratch dir, so it runs inside the Nix sandbox.
          doCheck = true;

          meta = {
            description = summary;
            homepage = "https://github.com/tenex-hq/claude-runway";
            license = pkgs.lib.licenses.mit;
            mainProgram = "claude-runway";
            platforms = systems;
          };
        };
        default = claude-runway;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [ pkgs.go pkgs.gopls pkgs.goreleaser ];
        };
      });

      # Lets `nix run github:tenex-hq/claude-runway` work without a build step.
      apps = forAllSystems (pkgs: {
        default = {
          type = "app";
          program = "${self.packages.${pkgs.stdenv.hostPlatform.system}.claude-runway}/bin/claude-runway";
          # `nix flake check` warns on apps without it.
          meta.description = summary;
        };
      });
    };
}
