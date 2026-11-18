{
  description = "Narcissus dev shell";

  # nixpkgs-unstable, pinned by flake.lock (the same pin as the platform repo).
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # Always the latest stable Go. nixpkgs lags behind Go's point releases,
      # so this builds the newest one from go.dev's source tarball, and builds
      # gopls, golangci-lint and govulncheck with it. Delete the override (back
      # to plain go_1_27) once nixpkgs ships this version or a newer one.
      goFor =
        pkgs:
        pkgs.go_1_27.overrideAttrs (_: rec {
          version = "1.27.4";
          src = pkgs.fetchurl {
            url = "https://go.dev/dl/go${version}.src.tar.gz";
            hash = "sha256-c8+4VxrsDAEXdofbhxY9Rqog6XeWcncokaoXZU2aYJk=";
          };
        });
    in
    {
      devShells = forAllSystems (
        pkgs:
        let
          go = goFor pkgs;
          buildGoModule = pkgs.buildGoModule.override { inherit go; };
          gopls = pkgs.gopls.override { buildGoLatestModule = buildGoModule; };
          golangci-lint = pkgs.golangci-lint.override { buildGo127Module = buildGoModule; };
          govulncheck = pkgs.govulncheck.override { buildGoLatestModule = buildGoModule; };
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              go
              gopls
              golangci-lint
              govulncheck
              gitleaks
              just
            ];

            # Use only the flake's Go, never a downloaded toolchain, and stay
            # cgo-free so every platform cross-compiles trivially.
            GOTOOLCHAIN = "local";
            CGO_ENABLED = "0";
          };
        }
      );

      # The CLI, built without cgo. Its tests run as the check, so
      # `nix build .#checks.aarch64-linux.test` tests it on Linux from a Mac.
      packages = forAllSystems (pkgs: {
        default = (pkgs.buildGoModule.override { go = goFor pkgs; }) {
          pname = "narcissus";
          version = self.shortRev or "dev";
          src = self;
          vendorHash = "sha256-7c3UPojUcKysE3q2UWc3lD1pscGVnNVbGTT+LoxnDVA=";
          env.CGO_ENABLED = "0";
          ldflags = [ "-X main.version=${self.shortRev or "dev"}" ];
          doCheck = false;
        };
      });

      checks = forAllSystems (pkgs: {
        test = self.packages.${pkgs.stdenv.hostPlatform.system}.default.overrideAttrs { doCheck = true; };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
