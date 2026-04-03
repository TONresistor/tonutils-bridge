{
  description = "TON Utils Bridge — WebSocket bridge for TON blockchain";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";
    gomod2nix.url = "github:nix-community/gomod2nix";
    gomod2nix.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs = { self, nixpkgs, gomod2nix }:
    let
      supportedSystems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" "x86_64-darwin" ];
      forEachSystem = nixpkgs.lib.genAttrs supportedSystems;
    in
    {
      packages = forEachSystem (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          inherit (gomod2nix.legacyPackages.${system}) buildGoApplication;
        in
        {
          default = buildGoApplication rec {
            pname = "tonutils-bridge";
            version = "0.1.0";

            src = builtins.path { path = ./.; name = "source"; };

            modules = ./gomod2nix.toml;

            pwd = src;

            subPackages = [ "." ];

            ldflags = [ "-s" "-w" ];

            meta = with pkgs.lib; {
              description = "WebSocket bridge for TON blockchain (tonutils-go)";
              homepage = "https://github.com/TONresistor/tonutils-bridge";
              license = licenses.mit; # adjust if LICENSE says otherwise
              mainProgram = "tonutils-bridge";
              platforms = supportedSystems;
            };
          };
        }
      );

      devShells = forEachSystem (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          bridge = self.packages.${system}.default;
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              go
              gopls
              (pkgs.writeShellScriptBin "run-bridge" ''
                exec ${bridge}/bin/tonutils-bridge "$@"
              '')
            ];

            shellHook = ''
              echo "🌉  tonutils-bridge dev shell"
              echo "  run-bridge [args…]  → run the built bridge"
            '';
          };
        }
      );
    };
}
