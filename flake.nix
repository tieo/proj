{
  description = "proj — tmux + Claude Code project session manager";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAll = f: nixpkgs.lib.genAttrs systems (s: f (import nixpkgs { system = s; }));
    in {
      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [ go gopls gotools go-tools tmux ];
        };
      });

      packages = forAll (pkgs: rec {
        proj = pkgs.buildGoModule {
          pname = "proj";
          version = "0.1.0";
          src = ./.;
          # vendorHash is set to null until `go mod tidy` produces a go.sum.
          # After the first successful `nix build`, replace with the hash nix prints.
          vendorHash = null;
          subPackages = [ "cmd/proj" ];
          meta = {
            description = "tmux + Claude Code project session manager";
            mainProgram = "proj";
            platforms = systems;
          };
        };
        default = proj;
      });
    };
}
