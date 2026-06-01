{
  description = "proj; tmux + Claude Code project session manager";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAll = f: nixpkgs.lib.genAttrs systems (s: f (import nixpkgs { system = s; }));
    in {
      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [ go gopls gotools tmux ];
        };
      });

      packages = forAll (pkgs: rec {
        proj = pkgs.buildGoModule {
          pname = "proj";
          version = "0.1.0";
          src = ./.;
          vendorHash = "sha256-Bz1u7u1Xk8UjIJqGJK0CGkFnT+baXP6LeskBgMpWJWo=";
          subPackages = [ "cmd/proj" ];
          postInstall = ''
            install -Dm0644 -t $out/share/proj shells/proj.zsh shells/proj.bash shells/proj.fish
          '';
          meta = {
            description = "tmux + Claude Code project session manager";
            mainProgram = "proj";
            platforms = systems;
            license = nixpkgs.lib.licenses.mit;
          };
        };
        default = proj;
      });
    };
}
